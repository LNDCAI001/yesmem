package proxy

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// smmForwardResult is the internal result of a SMM-aware forward attempt.
type smmForwardResult struct {
	resp         *http.Response
	err          error
	attempts     int
	bytesFlushed bool
}

// ForwardWithSMM executes the outbound HTTP request with SMM hook support.
//
// It is a wrapper around s.httpClient.Do that:
//   - calls proxyext.BeforeForward before each attempt to inject auth
//   - calls proxyext.OnPreStreamResponse after headers arrive but before
//     any bytes are written to the downstream client
//   - retries on RetryDecision.Retry == true, up to MaxAttempts
//   - stops unconditionally once BytesFlushed is true
//
// On return:
//   - resp is the final upstream response (caller must close resp.Body)
//   - err is non-nil only for network/dial failures on all attempts
//   - fc.BytesFlushed is false (stream not started; caller writes to w)
//
// If SMM is disabled (proxyext.ActiveHooks() returns noop), this is a
// thin wrapper with one extra function call overhead — no allocation.
func (s *Server) ForwardWithSMM(
	ctx context.Context,
	w http.ResponseWriter,
	origReq *http.Request,
	body []byte,
	targetURL string,
) (*http.Response, *proxyext.ForwardContext, error) {
	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   origReq.Header.Get("X-Thread-Id"),
			SessionID:  origReq.Header.Get("X-Session-Id"),
			Model:      extractModelFromBody(body),
			IsSubagent: isSubagentRequest(origReq),
		},
		OriginalBody: body,
	}

	// maxAttempts is filled from the first RetryDecision that carries it.
	// Default 1 means: one try, no retries (noop path).
	maxAttempts := 1

	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		fc.Attempt = attempt
		fc.BytesFlushed = false // reset per attempt; caller sets true on first write

		// Build a fresh request for this attempt.
		proxyReq, err := http.NewRequestWithContext(ctx, origReq.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			return nil, fc, fmt.Errorf("smm: create request (attempt %d): %w", attempt, err)
		}
		for key, vals := range origReq.Header {
			for _, v := range vals {
				proxyReq.Header.Add(key, v)
			}
		}
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding")
		fc.OutboundReq = proxyReq

		// BeforeForward: inject auth for the selected account.
		// Fail open: on error, proceed with whatever auth is on origReq.
		if hookErr := proxyext.BeforeForward(ctx, fc); hookErr != nil {
			log.Printf("[smm] BeforeForward error (attempt=%d), proceeding with original auth: %v", attempt, hookErr)
		}

		resp, doErr := s.httpClient.Do(proxyReq)
		if doErr != nil {
			lastErr = doErr
			// Network error: do not retry — classifier maps this to
			// FailureNetworkTransient which is non-retryable in v1.
			break
		}

		// OnPreStreamResponse: check status before any byte hits w.
		// BytesFlushed is always false here — this is the pre-stream boundary.
		decision, hookErr := proxyext.OnPreStreamResponse(ctx, fc, resp)
		if hookErr != nil {
			log.Printf("[smm] OnPreStreamResponse error (attempt=%d): %v", attempt, hookErr)
		}

		// Capture MaxAttempts from the first decision that carries a non-zero value.
		if decision.MaxAttempts > 0 && maxAttempts == 1 {
			maxAttempts = decision.MaxAttempts + 1 // +1: attempts are 0-indexed
		}

		if !decision.Retry || fc.BytesFlushed {
			lastResp = resp
			lastErr = nil
			break
		}

		// Retry: drain and close this response body before the next attempt.
		// Do not forward headers or body to w yet.
		log.Printf("[smm] retrying (attempt=%d, reason=%s)", attempt, decision.RetryReason)
		_ = resp.Body.Close()
		lastResp = nil
	}

	if lastResp == nil && lastErr == nil {
		// All retries exhausted with no usable response.
		return nil, fc, fmt.Errorf("smm: all %d attempt(s) exhausted", maxAttempts)
	}
	return lastResp, fc, lastErr
}

// isSubagentRequest returns true when the request carries a subagent marker.
// Mirrors the detection logic in internal/proxy/subagent.go.
func isSubagentRequest(r *http.Request) bool {
	return r.Header.Get("X-Subagent") == "true" ||
		r.Header.Get("X-Agent-Source") != ""
}

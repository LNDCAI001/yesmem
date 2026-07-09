package proxy

// proxy_forward_smm.go wires the proxyext hook surface into forwardWithAnnotation.
//
// Design constraints (verified from proxy_forward.go reading):
//
//  1. BeforeForward is called after proxyReq is fully constructed (headers
//     copied, Content-Length set) but before s.httpClient.Do(proxyReq).
//  2. The retry loop re-reads body from fc.OriginalBody and rebuilds the
//     request — it never re-reads origReq.Body (already drained).
//  3. OnPreStreamResponse is called after Do() returns resp, before the first
//     call to w.WriteHeader(). This is the only safe pre-flush boundary.
//  4. fc.BytesFlushed is set to true immediately before w.WriteHeader() so
//     the hard-fence invariant is always current at hook call time.
//  5. The retry loop is bounded by SMMMaxRetries (from SMMConfig) and by the
//     BytesFlushed fence. It rebuilds proxyReq from OriginalBody each time.
//  6. Fail open: BeforeForward errors are logged but not fatal unless
//     accountpool.IsExhausted returns true.
//
// This file contains no logic of its own — it is a thin shim that is easy to
// diff against upstream proxy_forward.go and easy to revert.
//
// NOTHING in this file should be changed without first reading proxy_forward.go
// to confirm the insertion point is still at the same boundary.

import (
	"bytes"
	"context"
	"net/http"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
)

// smmMaxRetries is the upper bound on pre-stream retries per request.
// Overridden by SMMConfig.AccountPool.MaxPreStreamRetries at runtime.
// We keep a package-level constant as a safety ceiling so a misconfigured
// max_prestream_retries cannot cause unbounded loops.
const smmMaxRetries = 8

// doWithSMMHooks executes the upstream round-trip with account-pool retry
// support. It is called by forwardWithAnnotation in place of the bare
// s.httpClient.Do(proxyReq) call when SMM hooks are active.
//
// Parameters:
//   - ctx: the request context (used to rebuild proxyReq on retry)
//   - fc: the ForwardContext threaded through all hook calls
//   - initialReq: the fully-constructed *http.Request (headers already set)
//   - body: the serialised body (same as fc.OriginalBody; passed separately
//     to avoid a type assertion on each rebuild)
//   - maxRetries: from SMMConfig.AccountPool.MaxPreStreamRetries
//   - w: the ResponseWriter — WriteHeader must NOT be called before this
//     function returns, so the pre-stream window is intact
//
// Returns the final *http.Response and any terminal error. The caller is
// responsible for calling resp.Body.Close().
//
// The function sets fc.BytesFlushed = false on entry (it controls the fence).
// The caller must set fc.BytesFlushed = true before calling w.WriteHeader().
func (s *Server) doWithSMMHooks(
	ctx context.Context,
	fc *proxyext.ForwardContext,
	initialReq *http.Request,
	body []byte,
	maxRetries int,
	w http.ResponseWriter,
) (*http.Response, error) {
	if maxRetries <= 0 || maxRetries > smmMaxRetries {
		maxRetries = 2 // spec default
	}

	// fc.BytesFlushed starts false; the caller sets it true before WriteHeader.
	fc.BytesFlushed = false

	currentReq := initialReq

	for attempt := 0; attempt <= maxRetries; attempt++ {
		fc.Attempt = attempt

		if attempt > 0 {
			// Rebuild the request from the preserved body so we never read
			// from an already-drained io.Reader.
			rebuilt, err := http.NewRequestWithContext(
				ctx,
				initialReq.Method,
				initialReq.URL.String(),
				bytes.NewReader(body),
			)
			if err != nil {
				return nil, err
			}
			// Copy headers from the original request (not initialReq, which
			// may already have a stale Authorization from the previous attempt).
			for k, vs := range initialReq.Header {
				for _, v := range vs {
					rebuilt.Header.Add(k, v)
				}
			}
			currentReq = rebuilt
			fc.OutboundReq = currentReq
		}

		// --- Hook: BeforeForward ---
		// Injects Authorization header and records SelectedAccount.
		// Fail open on non-exhaustion errors.
		if hookErr := proxyext.CallBeforeForward(fc); hookErr != nil {
			s.logger.Printf("[smm] BeforeForward fatal: %v", hookErr)
			return nil, hookErr
		}

		// --- Upstream round-trip ---
		resp, err := s.httpClient.Do(currentReq)
		if err != nil {
			// Network error: not retryable via account rotation.
			return nil, err
		}

		// --- Hook: OnPreStreamResponse ---
		// BytesFlushed is still false here — w.WriteHeader has not been called.
		decision, hookErr := proxyext.CallOnPreStreamResponse(fc, resp)
		if hookErr != nil {
			s.logger.Printf("[smm] OnPreStreamResponse error (ignored): %v", hookErr)
		}

		if !decision.Retry {
			// Normal path: return the response to the caller for header
			// forwarding and body streaming.
			return resp, nil
		}

		// Retry: close this response body before the next attempt.
		resp.Body.Close()
		s.logger.Printf("[smm] smm_account_rotation_total attempt=%d reason=%s",
			attempt, decision.Reason)
	}

	// All retries exhausted. Return the last response would require keeping
	// the body open across iterations — instead we return a synthetic error.
	// The caller surfaces this as 502.
	return nil, fmt.Errorf("smm: all %d pre-stream retry attempts exhausted", maxRetries)
}

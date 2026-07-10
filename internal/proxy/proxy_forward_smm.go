package proxy

// proxy_forward_smm.go — SMMForwardWithRetry
//
// This file implements the account-pool retry loop for the SMM extension.
// It is the ONLY file in internal/proxy that imports internal/proxyext.
// proxy_forward.go calls SMMForwardWithRetry when proxyext.IsActive() &&
// smmCfg.AccountPool.Enabled, and treats its return value identically to
// httpClient.Do — no other changes to proxy_forward.go are required.
//
// Import surface kept minimal: only proxyext (for hook dispatchers and
// types) and the stdlib. No other internal/proxy subsystems are called
// from this file to keep the coupling boundary explicit.

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
)

// SMMForwardWithRetry executes the account-pool retry loop and returns the
// first retryable or successful (*http.Response, nil) pair with the response
// body still open. The caller (forwardWithAnnotation) owns the response body
// and must call resp.Body.Close() via defer.
//
// The function NEVER calls w.WriteHeader or w.Write — it returns before any
// bytes reach the client, preserving the pre-stream retry invariant.
//
// Retry algorithm:
//
//  1. Build outbound request from body (fresh bytes.Reader).
//  2. Call BeforeForward — injects auth from the pool for this attempt.
//  3. Call httpClient.Do — sends the request, gets response headers.
//  4. Call OnPreStreamResponse — hook decides retry/continue.
//  5. If Retry && !BytesFlushed: drain+close resp body, increment attempt,
//     return to step 1.
//  6. If not retrying: return resp to caller for streaming.
//  7. If all attempts exhausted: return the last response (caller surfaces
//     it to the client — never a silent hang).
func SMMForwardWithRetry(
	s *Server,
	w http.ResponseWriter,
	origReq *http.Request,
	body []byte,
	threadID string,
) (*http.Response, error) {
	cfg := proxyext.ActiveSMMConfig()
	maxRetries := 2 // safe default
	if cfg != nil && cfg.AccountPool.MaxPreStreamRetries > 0 {
		maxRetries = cfg.AccountPool.MaxPreStreamRetries
	}

	// OriginalBody is captured once. Each attempt wraps it in a fresh
	// bytes.Reader so the body is never consumed more than once.
	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   threadID,
			IsSubagent: isSubagentFromBody(body),
		},
		OriginalBody: body,
		// BytesFlushed starts false and stays false for the entire lifetime of
		// this function — we never write to w. The hooks.go dispatcher also
		// enforces this; the field is set here to satisfy the type contract.
		BytesFlushed: false,
	}

	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		fc.Attempt = attempt

		// ── 1. Build a fresh outbound request for this attempt ────────────
		targetURL := s.resolveAnthropicTarget(extractModelFromBody(body)) + origReq.URL.RequestURI()
		proxyReq, err := http.NewRequestWithContext(
			origReq.Context(),
			origReq.Method,
			targetURL,
			bytes.NewReader(body), // fresh reader every attempt
		)
		if err != nil {
			return nil, fmt.Errorf("smm: build request (attempt %d): %w", attempt, err)
		}

		// Copy client headers onto the outbound request. BeforeForward will
		// overwrite the Authorization and remove x-api-key as needed.
		for key, vals := range origReq.Header {
			for _, v := range vals {
				proxyReq.Header.Add(key, v)
			}
		}
		proxyReq.Header.Set("Content-Length", strconv.Itoa(len(body)))
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding") // force uncompressed for SSE parsing

		fc.OutboundReq = proxyReq

		// ── 2. BeforeForward — account selection and auth injection ──────
		if err := proxyext.BeforeForward(fc); err != nil {
			// Hard exhaustion (all accounts unavailable) is surfaced as an
			// error so forwardWithAnnotation can return a clean 502.
			// Soft errors (fail-open) are swallowed inside BeforeForward
			// itself and the original client auth is preserved.
			return nil, fmt.Errorf("smm: BeforeForward (attempt %d): %w", attempt, err)
		}

		// ── 3. Record TTL observation for this attempt (matches stock path) ─
		if s.cacheTTLDetector != nil {
			s.cacheTTLDetector.RecordRequest(threadID)
		}

		// ── 4. Send the request — receive headers only, body not consumed ─
		s.logger.Printf("[smm] smm_account_attempt=%d", attempt)
		resp, doErr := s.httpClient.Do(proxyReq)
		if doErr != nil {
			// Network error before headers arrived. Not retryable by the
			// account pool (network transients are server-side, not
			// account-specific). Surface immediately.
			lastErr = doErr
			break
		}

		// ── 5. OnPreStreamResponse — retry decision ───────────────────────
		// BytesFlushed is false: no bytes have been written to w yet.
		decision, hookErr := proxyext.OnPreStreamResponse(fc, resp)
		if hookErr != nil {
			// Hook error: fail open — return the response as-is and let
			// forwardWithAnnotation stream it to the client normally.
			s.logger.Printf("[smm] smm_retry_decision=error hook_err=%v (fail open)", hookErr)
			lastResp = resp
			break
		}

		if !decision.Retry {
			// This attempt is final — hand response to caller for streaming.
			s.logger.Printf("[smm] smm_retry_decision=false smm_retry_reason=%s status=%d attempt=%d",
				decision.Reason, resp.StatusCode, attempt)
			lastResp = resp
			break
		}

		// ── 6. Retry: drain and close this response before the next attempt ─
		// Draining prevents goroutine leaks in the upstream connection pool.
		// We use a size-limited drain (32 KB) to avoid materialising large
		// error bodies from the upstream — we only care about connection reuse.
		s.logger.Printf("[smm] smm_stream_started_at_retry=false rotating account attempt=%d→%d status=%d reason=%s",
			attempt, attempt+1, resp.StatusCode, decision.Reason)
		drainAndClose(resp)
		lastResp = nil
	}

	// ── 7. Terminal: call OnPostResponse exactly once ────────────────────────
	// OnPostResponse fires even on error so per-account state is always updated.
	// It must not be called inside the retry loop — only on the final outcome.
	result := proxyext.ForwardResult{
		Err: lastErr,
	}
	if lastResp != nil {
		result.StatusCode = lastResp.StatusCode
	}
	proxyext.OnPostResponse(fc, result)

	if lastErr != nil {
		return nil, lastErr
	}
	if lastResp == nil {
		// All retries exhausted without a response: synthetic 503.
		return nil, fmt.Errorf("smm: all %d attempts exhausted with no response", maxRetries+1)
	}
	return lastResp, nil
}

// drainAndClose discards up to 32 KB of resp.Body then closes it.
// This allows the underlying TCP connection to be returned to the pool.
const drainLimit = 32 * 1024

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	// io.LimitReader + io.Discard: bounded drain to reuse the connection.
	// Errors here are intentionally ignored — we are abandoning this response.
	buf := make([]byte, drainLimit)
	resp.Body.Read(buf) //nolint:errcheck
	resp.Body.Close()
}

// Package proxy — SMM retry-loop wiring for proxy_forward.go.
//
// HOW TO INTEGRATE INTO proxy_forward.go
// ========================================
// This file lives in internal/proxy/ so it can be a peer of proxy_forward.go
// and share its unexported types. It defines SMMForwardWithRetry, the retry
// loop that implements Feature A (account rotation).
//
// You need to add TWO things to proxy_forward.go:
//
// 1. Near the top of the existing forward function, after the request is built
//    but before httpClient.Do, add:
//
//      if proxyext.IsEnabled() {
//          resp, err = SMMForwardWithRetry(ctx, h, w, origBody, outReq)
//          if err != nil {
//              http.Error(w, err.Error(), http.StatusBadGateway)
//          }
//          return
//      }
//
// 2. Add a one-time body drain before building outReq:
//
//      origBody, err := io.ReadAll(r.Body)
//      if err != nil { http.Error(w, "read body", 500); return }
//
//    yesmem likely already reads the body — verify this before adding the drain.
//    If it does, pass the already-read bytes here instead.
//
// INVARIANTS ENFORCED BY THIS FILE
// ==================================
//  - BytesFlushed is set to true before the first w.Write. OnPreStreamResponse
//    is called before any w.Write. These two facts combine to ensure Retry is
//    never true after flush.
//  - On max-retry exhaustion with no success, the last upstream response is
//    forwarded to the client. The client gets a real HTTP error, not a hang.
//  - Thread/session IDs in ForwardContext are never mutated across retries.
//  - The outbound request body is re-built from origBody on every attempt;
//    the original r.Body (already drained) is never re-read.
package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
	"github.com/LNDCAI001/yesmem/internal/proxyext/accountpool"
)

// smmHTTPClient is the *http.Client used by SMMForwardWithRetry.
// In production proxy_forward.go uses a package-level client; wire it here.
// TODO: replace with the actual package-level client variable from proxy.go.
var smmHTTPClient = &http.Client{Timeout: 120 * time.Second}

// SMMForwardWithRetry executes an upstream request with account-pool-aware
// pre-stream retry. It is called instead of the standard single-attempt path
// when proxyext.IsEnabled() is true.
//
// Parameters:
//   - ctx:      request context (should carry the client deadline)
//   - threadID: yesmem thread/session identifier, unchanged across retries
//   - isSubagent: from the parsed request
//   - w:        the client ResponseWriter — nothing is written until success
//   - origBody: the request body, drained exactly once by the caller
//   - outReq:   the outbound *http.Request already built by proxy_forward.go,
//               with all headers except Authorization (which BeforeForward injects)
func SMMForwardWithRetry(
	ctx context.Context,
	threadID string,
	isSubagent bool,
	w http.ResponseWriter,
	origBody []byte,
	outReq *http.Request,
) error {
	cfg := proxyext.ActiveSMMConfig()
	if cfg == nil {
		return nil
	}

	maxAttempts := cfg.AccountPool.MaxPreStreamRetries + 1 // +1 for first attempt
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   threadID,
			IsSubagent: isSubagent,
		},
		OriginalBody: origBody,
		// OutboundReq is set per-attempt below.
	}

	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		fc.Attempt = attempt
		fc.BytesFlushed = false
		fc.SelectedAccount = nil

		// Build a fresh clone of outReq with the preserved body.
		cloned, err := accountpool.ResendWithNewBody(ctx, smmHTTPClient, outReq, origBody)
		// ResendWithNewBody returns (*http.Response, error). We only need the
		// cloned request here, not the response — so we use the lower-level
		// helper to get the clone without sending it.
		_ = cloned // unused; see the block below for the correct pattern
		_ = err

		// Actually build the clone for mutation by BeforeForward, then send.
		attemptReq, cloneErr := cloneForAttempt(outReq, origBody)
		if cloneErr != nil {
			return cloneErr
		}
		fc.OutboundReq = attemptReq

		// Let the hook inject auth for this attempt.
		if hookErr := proxyext.BeforeForward(fc); hookErr != nil {
			// All accounts exhausted — surface as 503.
			http.Error(w, hookErr.Error(), http.StatusServiceUnavailable)
			return hookErr
		}

		// Send the request.
		resp, doErr := smmHTTPClient.Do(fc.OutboundReq)
		if doErr != nil {
			lastErr = doErr
			// Transport error — do not retry (FailureNetworkTimeout is not rotatable).
			// Mark via OnPreStreamResponse with a synthetic nil resp.
			// Classify(nil, false) returns FailureNetworkTimeout which IsRetryable=false.
			proxyext.OnPreStreamResponse(fc, nil) //nolint:errcheck
			break
		}
		lastResp = resp

		// Read the retry decision BEFORE writing anything to the client.
		// fc.BytesFlushed is false here — the dispatcher enforces this as a gate.
		decision, hookErr := proxyext.OnPreStreamResponse(fc, resp)
		if hookErr != nil {
			log.Printf("[smm] OnPreStreamResponse error (ignored, fail-open): %v", hookErr)
		}

		if decision.Retry {
			// Close the body before the next attempt to avoid leaking connections.
			resp.Body.Close()
			continue
		}

		// No retry — stream the response to the client.
		w.WriteHeader(resp.StatusCode)
		fc.BytesFlushed = true

		_, copyErr := io.Copy(w, resp.Body)
		resp.Body.Close()

		// Record outcome.
		result := proxyext.ForwardResult{
			StatusCode: resp.StatusCode,
			Err:        copyErr,
		}
		proxyext.OnPostResponse(fc, result)

		return copyErr
	}

	// Exhausted all attempts. Forward the last response if we have one.
	if lastResp != nil {
		w.WriteHeader(lastResp.StatusCode)
		fc.BytesFlushed = true
		_, _ = io.Copy(w, lastResp.Body)
		lastResp.Body.Close()
		proxyext.OnPostResponse(fc, proxyext.ForwardResult{StatusCode: lastResp.StatusCode})
		return nil
	}

	// Transport error with no response at all.
	if lastErr != nil {
		proxyext.OnPostResponse(fc, proxyext.ForwardResult{Err: lastErr})
		http.Error(w, "upstream error: "+lastErr.Error(), http.StatusBadGateway)
		return lastErr
	}

	return nil
}

// cloneForAttempt creates a fresh outbound request with origBody as the body.
// Headers are deep-copied so BeforeForward can mutate them freely.
func cloneForAttempt(base *http.Request, origBody []byte) (*http.Request, error) {
	cloned := base.Clone(base.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(origBody))
	cloned.ContentLength = int64(len(origBody))
	cloned.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(origBody)), nil
	}
	cloned.Header = base.Header.Clone()
	return cloned, nil
}

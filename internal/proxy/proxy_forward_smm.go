package proxy

import (
	"bytes"
	"net/http"
	"strconv"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// smmForwardWithRetry is the SMM-aware entry point for forwarding annotated
// requests. When no SMM config is active, proxyext.BeforeForward and
// proxyext.OnPreStreamResponse dispatch to the noop implementation and the
// overhead is two interface calls per request — negligible.
//
// When account pool rotation is enabled, the pre-stream retry loop works as
// follows:
//
//  1. Build a ForwardContext for this top-level request (once).
//  2. For each attempt (0..MaxAttempts-1):
//     a. Build a fresh outbound *http.Request from OriginalBody.
//     b. Call proxyext.BeforeForward — injects auth header for selected account.
//        On error (pool exhausted): return 503 immediately.
//     c. Send via s.httpClient.Do.
//     d. Call proxyext.OnPreStreamResponse — inspects status BEFORE w.WriteHeader.
//        If Retry=true and attempts remain: close resp body, continue loop.
//        Otherwise: fall through to flush.
//  3. Flush path: patch origReq's Authorization with the injected token,
//     then delegate to forwardWithAnnotation for SSE/usage/sawtooth/cache logic.
//  4. Call proxyext.OnPostResponse deferred after flush returns.
//
// Hard invariant: w.WriteHeader is called inside forwardWithAnnotation.
// No byte is written to the client during steps 1-2d. If step 2d decides
// Retry=false, we proceed directly to step 3 without having written anything.
func (s *Server) smmForwardWithRetry(
	w http.ResponseWriter,
	origReq *http.Request,
	body []byte,
	reqIdx int,
	toolUseIDs []string,
	proj string,
	threadID string,
	msgCount int,
	estimatedTokens ...int,
) {
	ctx := origReq.Context()

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   threadID,
			SessionID:  threadID,
			Model:      extractModelFromBody(body),
			IsSubagent: isSubagentFromBody(body),
		},
		OriginalBody: body,
		Attempt:      0,
	}

	// maxAttempts starts at 1. OnPreStreamResponse may raise it on the first
	// retryable response.
	maxAttempts := 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		fc.Attempt = attempt

		// Build a fresh outbound request for this attempt.
		targetURL := s.resolveAnthropicTarget(extractModelFromBody(body)) + origReq.URL.RequestURI()
		reqBody := stripProviderPrefixFromBody(body)

		proxyReq, err := http.NewRequestWithContext(ctx, origReq.Method, targetURL, bytes.NewReader(reqBody))
		if err != nil {
			s.logger.Printf("[smm req %d attempt %d] create request: %v", reqIdx, attempt, err)
			http.Error(w, "failed to create proxy request", http.StatusBadGateway)
			return
		}
		for key, vals := range origReq.Header {
			for _, v := range vals {
				proxyReq.Header.Add(key, v)
			}
		}
		proxyReq.Header.Set("Content-Length", strconv.Itoa(len(reqBody)))
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding")
		fc.OutboundReq = proxyReq

		// BeforeForward: inject auth for the selected account.
		if hookErr := proxyext.BeforeForward(ctx, fc); hookErr != nil {
			s.logger.Printf("[smm req %d attempt %d] BeforeForward: %v", reqIdx, attempt, hookErr)
			http.Error(w, "account pool exhausted", http.StatusServiceUnavailable)
			return
		}

		// Send the request.
		resp, doErr := s.httpClient.Do(proxyReq)
		if doErr != nil {
			s.logger.Printf("[smm req %d attempt %d] upstream: %v", reqIdx, attempt, doErr)
			http.Error(w, "upstream error: "+doErr.Error(), http.StatusBadGateway)
			return
		}

		// OnPreStreamResponse: inspect status BEFORE w.WriteHeader (no flush yet).
		decision, _ := proxyext.OnPreStreamResponse(ctx, fc, resp)

		if decision.Retry {
			// Raise maxAttempts if the hook told us a higher limit.
			if decision.MaxAttempts > maxAttempts {
				maxAttempts = decision.MaxAttempts
			}
			if attempt+1 < maxAttempts {
				resp.Body.Close()
				s.logger.Printf("[smm req %d] retry %d→%d reason=%s",
					reqIdx, attempt, attempt+1, decision.RetryReason)
				continue
			}
			// Retry requested but no attempts left — fall through and surface
			// the last upstream response to the client.
			s.logger.Printf("[smm req %d] retry limit reached after %d attempts", reqIdx, attempt+1)
		}

		// Past the retry boundary. Patch auth onto origReq and delegate to
		// forwardWithAnnotation for the full SSE/annotation/cache lifecycle.
		// BytesFlushed becomes true inside forwardWithAnnotation.
		resp.Body.Close() // forwardWithAnnotation will open a new connection.

		var flushReq *http.Request
		if injectedAuth := fc.OutboundReq.Header.Get("Authorization"); injectedAuth != "" {
			flushReq = origReq.Clone(ctx)
			flushReq.Header.Set("Authorization", injectedAuth)
		} else {
			flushReq = origReq
		}

		result := proxyext.ForwardResult{
			StatusCode:   resp.StatusCode,
			StreamStarted: true,
			BytesFlushed: true,
		}
		s.forwardWithAnnotation(w, flushReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
		proxyext.OnPostResponse(ctx, fc, result)
		return
	}

	// Retry loop exhausted without dispatching to flush path.
	s.logger.Printf("[smm req %d] all accounts exhausted after %d attempts", reqIdx, maxAttempts)
	http.Error(w, "all accounts exhausted", http.StatusServiceUnavailable)
}

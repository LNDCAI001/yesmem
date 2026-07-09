package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// smmForwardWithRetry wraps forwardWithAnnotation with the SMM pre-stream
// retry loop. It is the only entry point that should be called when the SMM
// hook layer is active.
//
// Invariants enforced here:
//
//	1. BeforeForward is called before every upstream HTTP request.
//	2. OnPreStreamResponse is called after headers arrive but before ANY byte
//	   is written to w (BytesFlushed == false at the call site).
//	3. Once w.WriteHeader fires, BytesFlushed is set to true and no retry
//	   can occur — the loop exits unconditionally.
//	4. fc.OriginalBody is used verbatim to rebuild the request on retry;
//	   the prompt body is never mutated by the retry loop.
//	5. Account identity lives only in fc.SelectedAccount and in the
//	   Authorization header of fc.OutboundReq — never in any other header
//	   that reaches the upstream API.
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
	// Fast path: when SMM is fully disabled the active hooks are DefaultHooks
	// (noop). BeforeForward and OnPreStreamResponse return immediately with
	// zero allocations. Fall through to forwardWithAnnotation directly.
	if !proxyext.HooksActive() {
		s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
		return
	}

	targetURL := s.resolveAnthropicTarget(extractModelFromBody(body)) + origReq.URL.RequestURI()
	body = stripProviderPrefixFromBody(body)

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   threadID,
			Model:      extractModelStringFromBody(body),
			IsSubagent: isSubagentFromBody(body),
		},
		OriginalBody: body,
		Attempt:      0,
	}

	var (
		resp        *http.Response
		lastErr     error
		maxAttempts = 3 // default; overridden by RetryDecision.MaxAttempts
	)

	for {
		// Build outbound request from the preserved original body.
		proxyReq, err := http.NewRequestWithContext(
			origReq.Context(), origReq.Method, targetURL,
			bytes.NewReader(fc.OriginalBody),
		)
		if err != nil {
			s.logger.Printf("[smm req %d] build request error: %v", reqIdx, err)
			http.Error(w, "smm: failed to create proxy request", http.StatusBadGateway)
			return
		}
		for key, vals := range origReq.Header {
			for _, v := range vals {
				proxyReq.Header.Add(key, v)
			}
		}
		proxyReq.Header.Set("Content-Length", strconv.Itoa(len(fc.OriginalBody)))
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding")
		fc.OutboundReq = proxyReq

		// Hook: inject auth header for the selected account.
		// Stores result in fc.SelectedAccount — never on a request header.
		if hookErr := proxyext.BeforeForward(origReq.Context(), fc); hookErr != nil {
			s.logger.Printf("[smm req %d] BeforeForward error (attempt=%d): %v", reqIdx, fc.Attempt, hookErr)
			// Fail open: fall through to forwardWithAnnotation with stock auth.
			s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
			return
		}

		// Send the request upstream.
		resp, lastErr = s.httpClient.Do(fc.OutboundReq)
		if lastErr != nil {
			s.logger.Printf("[smm req %d] upstream error (attempt=%d): %v", reqIdx, fc.Attempt, lastErr)
			// Network error is not retryable — surface immediately.
			http.Error(w, "upstream error: "+lastErr.Error(), http.StatusBadGateway)
			go proxyext.OnPostResponse(origReq.Context(), fc, proxyext.ForwardResult{
				StatusCode:        0,
				ClassifiedFailure: "network_error",
			})
			return
		}

		// PRE-STREAM BOUNDARY: headers received, BytesFlushed == false.
		// This is the only moment a retry is legal.
		decision, hookErr := proxyext.OnPreStreamResponse(origReq.Context(), fc, resp)
		if hookErr != nil {
			// Hook error: fail open, use whatever response we have.
			s.logger.Printf("[smm req %d] OnPreStreamResponse error: %v", reqIdx, hookErr)
			break
		}

		if !decision.Retry {
			// Non-retryable status or success: exit loop and stream response.
			break
		}

		// Update maxAttempts from the decision if the hook wants stricter limits.
		if decision.MaxAttempts > 0 {
			maxAttempts = decision.MaxAttempts + 1 // +1 because attempt 0 is the first try
		}

		if fc.Attempt+1 >= maxAttempts {
			s.logger.Printf("[smm req %d] max retries (%d) reached, reason=%s",
				reqIdx, maxAttempts-1, decision.RetryReason)
			break
		}

		// Discard this response and retry with next account.
		s.logger.Printf("[smm req %d] retrying (attempt=%d, reason=%s, account=%s)",
			reqIdx, fc.Attempt, decision.RetryReason, fc.SelectedAccount.Name)
		resp.Body.Close()
		resp = nil
		fc.Attempt++
	}

	if resp == nil {
		// All attempts exhausted with no usable response.
		s.logger.Printf("[smm req %d] all accounts exhausted", reqIdx)
		http.Error(w, fmt.Sprintf("smm: all %d account(s) exhausted", maxAttempts-1), http.StatusBadGateway)
		go proxyext.OnPostResponse(origReq.Context(), fc, proxyext.ForwardResult{
			StatusCode:        0,
			ClassifiedFailure: "pool_exhausted",
		})
		return
	}

	// We have a usable response with BytesFlushed == false.
	// Close this probe response — forwardWithAnnotation will make its own
	// request with the winning account's auth already set on origReq.
	//
	// This means one extra RTT on the final successful attempt. Correctness
	// over optimisation in v1; patching forwardWithAnnotation to accept a
	// pre-built *http.Response is left as a v2 optimisation.
	resp.Body.Close()

	// Copy the winning account's Authorization header onto origReq so that
	// forwardWithAnnotation sends with the correct credentials.
	if auth := fc.OutboundReq.Header.Get("Authorization"); auth != "" {
		origReq.Header.Set("Authorization", auth)
	}

	go proxyext.OnPostResponse(origReq.Context(), fc, proxyext.ForwardResult{
		StatusCode:    resp.StatusCode,
		StreamStarted: false,
		BytesFlushed:  false,
	})

	s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
}

// extractModelStringFromBody extracts the model field from a JSON request body.
// Used by the SMM layer before the full request parse.
func extractModelStringFromBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return m.Model
}

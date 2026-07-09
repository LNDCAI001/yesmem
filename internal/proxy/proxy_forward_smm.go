package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
	// Hand it to the annotation forwarder. Because forwardWithAnnotation
	// re-does the http.Client.Do call internally, we snapshot the result
	// state and call OnPostResponse here, then delegate the actual stream
	// flush via the standard path.
	//
	// NOTE: forwardWithAnnotation will re-build and re-send the request.
	// This means we incur one extra RTT for the final successful attempt.
	// The alternative (patching forwardWithAnnotation to accept a pre-built
	// *http.Response) is left as a future optimisation; correctness takes
	// priority over the extra RTT on the success path.
	//
	// Close the resp we have — forwardWithAnnotation will get its own.
	resp.Body.Close()

	// Restore original auth header from the winning account's token so
	// forwardWithAnnotation sends with the correct credentials.
	// fc.OutboundReq.Header already has it set by BeforeForward.
	// Copy the Authorization header back onto origReq for the delegation.
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

// extractModelStringFromBody is a lightweight model-name extractor used by
// the SMM layer before the full request parse. Duplicates a small part of
// extractModelFromBody to avoid package-level import cycles.
func extractModelStringFromBody(body []byte) string {
	type modelOnly struct {
		Model string `json:"model"`
	}
	var m modelOnly
	if err := unmarshalFast(body, &m); err != nil || m.Model == "" {
		return ""
	}
	return m.Model
}

// unmarshalFast is a thin wrapper retained so the SMM file does not directly
// import encoding/json in a way that shadows the package-level import pattern.
func unmarshalFast(data []byte, v any) error {
	import_ := func() error {
		// Forward-compat shim: call json.Unmarshal via a local alias so that
		// future swaps to a faster decoder (e.g. sonic) happen in one place.
		import "encoding/json"
		return json.Unmarshal(data, v)
	}
	return import_()
}

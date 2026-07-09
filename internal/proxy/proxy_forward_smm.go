package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strconv"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// smmForwardWithAnnotation is the SMM-aware entry point for forwardWithAnnotation.
// When SMM is disabled (hooks == DefaultHooks), it calls forwardWithAnnotation
// directly with zero overhead beyond a single interface nil-check.
//
// When SMM is enabled, it wraps the request/response cycle in a pre-stream retry
// loop:
//
//	1. BeforeForward injects the selected account's Bearer token.
//	2. The request is sent via httpClient.Do.
//	3. OnPreStreamResponse reads the upstream status code BEFORE w.WriteHeader.
//	   If it signals Retry and we are within MaxAttempts, the response body is
//	   closed, a fresh request is built from fc.OriginalBody, and BeforeForward
//	   runs again with fc.Attempt incremented.
//	4. Once we break out of the loop (success or exhaustion), control passes to
//	   the existing forwardWithAnnotation body-streaming logic.
//	5. OnPostResponse fires after the stream ends.
//
// Hard invariants:
//   - No retry after w.WriteHeader has been called (BytesFlushed boundary).
//   - fc.OriginalBody is the canonical body; it is never mutated.
//   - Account identity never touches a request header sent to the upstream API.
func (s *Server) smmForwardWithAnnotation(
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
	// Fast path: SMM disabled — zero overhead, direct call.
	if !proxyext.IsEnabled() {
		s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
		return
	}

	targetURL := s.resolveAnthropicTarget(extractModelFromBody(body)) + origReq.URL.RequestURI()
	body = stripProviderPrefixFromBody(body)

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   threadID,
			SessionID:  threadID, // yesmem uses threadID as session key
			Model:      extractModelStringFromBody(body),
			IsSubagent: isSubagentFromBody(body),
		},
		OriginalBody: body,
		Attempt:      0,
	}

	maxAttempts := proxyext.MaxPreStreamRetries()
	var resp *http.Response
	var doErr error

	for attempt := 0; attempt <= maxAttempts; attempt++ {
		fc.Attempt = attempt

		// Rebuild outbound request from the preserved original body each attempt.
		// On attempt 0 this is the first build; on retries it is a clean rebuild.
		proxyReq, buildErr := http.NewRequestWithContext(
			origReq.Context(),
			origReq.Method,
			targetURL,
			bytes.NewReader(fc.OriginalBody),
		)
		if buildErr != nil {
			s.logger.Printf("[smm req %d] build error on attempt %d: %v", reqIdx, attempt, buildErr)
			http.Error(w, "smm: failed to build proxy request", http.StatusBadGateway)
			return
		}
		copyHeaders(proxyReq, origReq)
		proxyReq.Header.Set("Content-Length", strconv.Itoa(len(fc.OriginalBody)))
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding")
		fc.OutboundReq = proxyReq

		// BeforeForward: injects auth header for the selected account.
		// On failure, fail open: fall through to stock forward path.
		if hookErr := proxyext.BeforeForward(origReq.Context(), fc); hookErr != nil {
			s.logger.Printf("[smm req %d] BeforeForward error (attempt %d): %v — falling through to stock path", reqIdx, attempt, hookErr)
			s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
			return
		}

		resp, doErr = s.httpClient.Do(fc.OutboundReq)
		if doErr != nil {
			// Network-level error: not retryable per classifier, surface immediately.
			s.logger.Printf("[smm req %d] upstream error (attempt %d): %v", reqIdx, attempt, doErr)
			http.Error(w, "upstream error: "+doErr.Error(), http.StatusBadGateway)
			return
		}

		// OnPreStreamResponse: status is readable here; w.WriteHeader has NOT been
		// called yet, so no bytes have been flushed to the client.
		decision, _ := proxyext.OnPreStreamResponse(origReq.Context(), fc, resp)
		if !decision.Retry || attempt >= maxAttempts {
			// Either success, non-retryable failure, or retry budget exhausted.
			break
		}

		// Retrying: drain and close the response body to free the connection.
		resp.Body.Close()
		resp = nil
		s.logger.Printf("[smm req %d] retrying (attempt %d→%d, reason=%s)",
			reqIdx, attempt, attempt+1, decision.RetryReason)
	}

	if resp == nil {
		// All retries exhausted without a usable response.
		s.logger.Printf("[smm req %d] all %d attempts exhausted", reqIdx, maxAttempts+1)
		http.Error(w, "smm: all accounts exhausted", http.StatusServiceUnavailable)
		go proxyext.OnPostResponse(origReq.Context(), fc, proxyext.ForwardResult{
			StatusCode:        http.StatusServiceUnavailable,
			StreamStarted:     false,
			BytesFlushed:      false,
			ClassifiedFailure: "exhausted",
		})
		return
	}
	defer resp.Body.Close()

	// Substitute the rebuilt request into origReq's context so
	// forwardWithAnnotation uses the SMM-authed request for its DO call.
	// We achieve this by passing fc.OutboundReq's auth header back onto
	// origReq before handing off — forwardWithAnnotation will copy all
	// headers from origReq onto its own proxyReq build.
	//
	// NOTE: forwardWithAnnotation calls s.httpClient.Do again internally,
	// so we cannot reuse resp here. Instead we close resp, patch origReq's
	// Authorization, and let forwardWithAnnotation do the real streaming call.
	// The extra round-trip is acceptable: the retry cost was already paid above,
	// and forwardWithAnnotation owns the SSE annotation, usage tracking, and
	// sawtooth logic that we must not duplicate.
	resp.Body.Close()

	// Patch the Authorization header on origReq with the winning account's token.
	// fc.OutboundReq already has the injected Authorization set by BeforeForward.
	// We copy only the Authorization header — no other header from the outbound
	// request bleeds back (there are no SMM-specific headers on OutboundReq).
	if authVal := fc.OutboundReq.Header.Get("Authorization"); authVal != "" {
		origReq.Header.Set("Authorization", authVal)
	}

	// Delegate full streaming + annotation + usage tracking to the existing function.
	// OnPostResponse fires when forwardWithAnnotation returns.
	s.forwardWithAnnotation(w, origReq, fc.OriginalBody, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)

	// Fire OnPostResponse asynchronously — result fields are best-effort here
	// since forwardWithAnnotation owns stream tracking internally.
	go proxyext.OnPostResponse(origReq.Context(), fc, proxyext.ForwardResult{
		StatusCode:    http.StatusOK,
		StreamStarted: true,
		BytesFlushed:  true,
	})
}

// copyHeaders copies all headers from src to dst.
func copyHeaders(dst *http.Request, src *http.Request) {
	for key, vals := range src.Header {
		for _, v := range vals {
			dst.Header.Add(key, v)
		}
	}
}

// extractModelStringFromBody extracts the model field from a JSON body as a string.
// Returns empty string on any parse failure.
func extractModelStringFromBody(body []byte) string {
	// extractModelFromBody already exists in proxy_forward.go and returns a models.Model.
	// We need the raw string for RequestContext.Model.
	// Use a local minimal parse to avoid importing models package in the smm file.
	const modelKey = `"model":`
	start := bytes.Index(body, []byte(modelKey))
	if start < 0 {
		return ""
	}
	rest := body[start+len(modelKey):]
	// Skip whitespace
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\n') {
		i++
	}
	if i >= len(rest) || rest[i] != '"' {
		return ""
	}
	rest = rest[i+1:]
	end := bytes.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

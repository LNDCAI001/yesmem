package proxy

import (
	"bytes"
	"net/http"
	"strconv"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// smmForwardWithAnnotation is the SMM-aware entry point for forwardWithAnnotation.
// When SMM is disabled (HooksActive() == false), it calls forwardWithAnnotation
// directly with zero overhead.
//
// When SMM is enabled, it wraps the request/response cycle in a pre-stream retry
// loop:
//
//  1. BeforeForward injects the selected account's Bearer token into OutboundReq.
//     The AccountRef is stored in fc.SelectedAccount, never in a request header.
//  2. The request is sent via httpClient.Do.
//  3. OnPreStreamResponse reads the upstream status code BEFORE w.WriteHeader.
//     If it signals Retry and we are within MaxAttempts, the response body is
//     closed, a fresh request is built from fc.OriginalBody, and BeforeForward
//     runs again with fc.Attempt incremented.
//  4. Once we break out of the loop (success, non-retryable, or budget exhausted),
//     we patch origReq.Authorization with the winning account's token and delegate
//     all streaming/annotation/usage tracking to forwardWithAnnotation.
//  5. OnPostResponse fires after forwardWithAnnotation returns.
//
// Hard invariants:
//   - No retry after w.WriteHeader has been called (BytesFlushed boundary).
//   - fc.OriginalBody is the canonical body; it is never mutated.
//   - Account identity never appears in any header sent to the upstream API.
//   - On any hook error, we fail open to the stock forwardWithAnnotation path.
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
	// Fast path: SMM disabled — zero overhead.
	if !proxyext.HooksActive() {
		s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
		return
	}

	targetURL := s.resolveAnthropicTarget(extractModelFromBody(body)) + origReq.URL.RequestURI()
	body = stripProviderPrefixFromBody(body)

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   threadID,
			SessionID:  threadID, // yesmem uses threadID as session key
			Model:      smmExtractModel(body),
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

		// Build a fresh outbound request from fc.OriginalBody each attempt.
		// On retries this ensures the body reader is rewound.
		proxyReq, buildErr := http.NewRequestWithContext(
			origReq.Context(),
			origReq.Method,
			targetURL,
			bytes.NewReader(fc.OriginalBody),
		)
		if buildErr != nil {
			s.logger.Printf("[smm req %d] build error attempt %d: %v", reqIdx, attempt, buildErr)
			http.Error(w, "smm: failed to build proxy request", http.StatusBadGateway)
			return
		}
		smmCopyHeaders(proxyReq, origReq)
		proxyReq.Header.Set("Content-Length", strconv.Itoa(len(fc.OriginalBody)))
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding")
		fc.OutboundReq = proxyReq

		// BeforeForward: injects auth for selected account into fc.OutboundReq.
		// On hook error: fail open — fall through to stock path.
		if hookErr := proxyext.BeforeForward(origReq.Context(), fc); hookErr != nil {
			s.logger.Printf("[smm req %d] BeforeForward error attempt %d: %v — falling through", reqIdx, attempt, hookErr)
			s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
			return
		}

		resp, doErr = s.httpClient.Do(fc.OutboundReq)
		if doErr != nil {
			s.logger.Printf("[smm req %d] upstream error attempt %d: %v", reqIdx, attempt, doErr)
			http.Error(w, "upstream error: "+doErr.Error(), http.StatusBadGateway)
			return
		}

		// OnPreStreamResponse: status readable here; w.WriteHeader NOT yet called.
		// This is the only safe retry window.
		decision, _ := proxyext.OnPreStreamResponse(origReq.Context(), fc, resp)
		if !decision.Retry || attempt >= maxAttempts {
			break
		}

		// Retrying: drain and close to release the connection back to the pool.
		resp.Body.Close()
		resp = nil
		s.logger.Printf("[smm req %d] retry %d→%d reason=%s", reqIdx, attempt, attempt+1, decision.RetryReason)
	}

	if resp == nil {
		s.logger.Printf("[smm req %d] all %d attempts exhausted", reqIdx, maxAttempts+1)
		http.Error(w, "smm: all accounts exhausted", http.StatusServiceUnavailable)
		go proxyext.OnPostResponse(origReq.Context(), fc, proxyext.ForwardResult{
			StatusCode:        http.StatusServiceUnavailable,
			ClassifiedFailure: "exhausted",
		})
		return
	}

	// We have a good response from the winning account attempt.
	// Close this probe response — forwardWithAnnotation will issue the real
	// streaming call. Patch origReq's Authorization so it uses the winning
	// account's token for that call.
	resp.Body.Close()
	if authVal := fc.OutboundReq.Header.Get("Authorization"); authVal != "" {
		origReq.Header.Set("Authorization", authVal)
	}

	// Delegate full SSE streaming, annotation, usage tracking, sawtooth, and
	// cache keepalive to the existing function. This avoids duplicating any
	// of that logic here.
	s.forwardWithAnnotation(w, origReq, fc.OriginalBody, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)

	// Fire OnPostResponse asynchronously after the stream ends.
	go proxyext.OnPostResponse(origReq.Context(), fc, proxyext.ForwardResult{
		StatusCode:   http.StatusOK,
		StreamStarted: true,
		BytesFlushed: true,
	})
}

// smmCopyHeaders copies all headers from src to dst.
// Named smmCopyHeaders (not copyHeaders) to avoid collision with any future
// upstream helper of that name in the proxy package.
func smmCopyHeaders(dst *http.Request, src *http.Request) {
	for key, vals := range src.Header {
		for _, v := range vals {
			dst.Header.Add(key, v)
		}
	}
}

// smmExtractModel extracts the model string from the JSON body.
// Named smmExtractModel to avoid collision with upstream extractModelFromBody
// which returns a models.Model struct rather than a bare string.
func smmExtractModel(body []byte) string {
	const key = `"model":`
	start := bytes.Index(body, []byte(key))
	if start < 0 {
		return ""
	}
	rest := body[start+len(key):]
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

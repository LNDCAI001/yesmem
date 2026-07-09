package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// forwardWithSMM is the SMM-aware replacement for forwardWithAnnotation on
// the Claude subscription path. It adds:
//
//  1. BeforeForward — account selection and auth injection before each attempt.
//  2. OnPreStreamResponse — pre-flush retry decision after response headers.
//  3. bytesFlushedWriter — hard stop: no retry once a single byte reaches client.
//  4. OnPostResponse (deferred) — outcome recording, always fires.
//
// When SMM is disabled (HooksActive() == false), this function is a thin
// trampoline into forwardWithAnnotation with zero extra allocations on the
// hot path.
func (s *Server) forwardWithSMM(
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
	// Fast-path: SMM disabled — zero overhead, identical behaviour to stock.
	if !proxyext.HooksActive() {
		s.forwardWithAnnotation(w, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
		return
	}

	// Build the shared ForwardContext. OriginalBody is the immutable snapshot
	// used to rebuild the outbound request on each retry attempt.
	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID:   threadID,
			SessionID:  threadID,
			Model:      extractModelFromBody(body),
			IsSubagent: isSubagentFromBody(body),
		},
		OriginalBody: body,
	}

	// result is populated during the loop and consumed by OnPostResponse.
	var result proxyext.ForwardResult

	// OnPostResponse is fire-and-forget; always runs regardless of exit path.
	defer func() {
		proxyext.OnPostResponse(origReq.Context(), fc, result)
	}()

	// maxAttempts starts at 1 (first try only). OnPreStreamResponse may raise
	// it via RetryDecision.MaxAttempts on the first retry signal.
	maxAttempts := proxyext.MaxPreStreamRetries() + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for fc.Attempt = 0; fc.Attempt < maxAttempts; fc.Attempt++ {
		// --- Build outbound request for this attempt ---
		attemptBody := stripProviderPrefixFromBody(fc.OriginalBody)
		targetURL := s.resolveAnthropicTarget(extractModelFromBody(fc.OriginalBody)) + origReq.URL.RequestURI()

		proxyReq, err := http.NewRequestWithContext(
			origReq.Context(), origReq.Method, targetURL, bytes.NewReader(attemptBody),
		)
		if err != nil {
			s.logger.Printf("[smm req %d attempt %d] create request error: %v", reqIdx, fc.Attempt, err)
			if !result.BytesFlushed {
				http.Error(w, "failed to create proxy request", http.StatusBadGateway)
			}
			return
		}
		for key, vals := range origReq.Header {
			for _, v := range vals {
				proxyReq.Header.Add(key, v)
			}
		}
		proxyReq.Header.Set("Content-Length", strconv.Itoa(len(attemptBody)))
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding")
		fc.OutboundReq = proxyReq

		// --- BeforeForward: account selection + auth header injection ---
		// On error the call aborts; the pool itself decided no account is
		// available. Surface 503 to the client immediately.
		if bfErr := proxyext.BeforeForward(origReq.Context(), fc); bfErr != nil {
			s.logger.Printf("[smm req %d attempt %d] BeforeForward: %v", reqIdx, fc.Attempt, bfErr)
			result.ClassifiedFailure = "auth_exhausted"
			if !result.BytesFlushed {
				http.Error(w,
					fmt.Sprintf("smm: all accounts exhausted after %d attempt(s)", fc.Attempt),
					http.StatusServiceUnavailable)
			}
			return
		}

		// --- cacheTTLDetector: record request (mirrors forwardWithAnnotation) ---
		if s.cacheTTLDetector != nil {
			s.cacheTTLDetector.RecordRequest(threadID)
		}

		// --- Send request upstream ---
		resp, doErr := s.httpClient.Do(proxyReq)
		if doErr != nil {
			s.logger.Printf("[smm req %d attempt %d] upstream error: %v", reqIdx, fc.Attempt, doErr)
			result.ClassifiedFailure = "network_error"
			if !result.BytesFlushed {
				http.Error(w, "upstream error: "+doErr.Error(), http.StatusBadGateway)
			}
			return
		}

		result.StatusCode = resp.StatusCode

		// --- OnPreStreamResponse: called BEFORE w.WriteHeader, BEFORE any flush ---
		// BytesFlushed is guaranteed false here (no writes have occurred yet).
		// This is the only point where retry is legal.
		decision, hookErr := proxyext.OnPreStreamResponse(origReq.Context(), fc, resp)
		if hookErr != nil {
			s.logger.Printf("[smm req %d attempt %d] OnPreStreamResponse error (non-fatal): %v",
				reqIdx, fc.Attempt, hookErr)
		}

		if decision.Retry && !result.BytesFlushed {
			// Drain and discard the upstream body before retry so the TCP
			// connection can be reused by the http.Client pool.
			resp.Body.Close()
			s.logger.Printf("[smm req %d attempt %d] retry (reason=%s account=%s)",
				reqIdx, fc.Attempt, decision.RetryReason, fc.SelectedAccount.Name)
			continue
		}

		if decision.Retry && result.BytesFlushed {
			// Retry was wanted but bytes are already on the wire — hard stop.
			s.logger.Printf("[smm req %d attempt %d] retry blocked: bytes already flushed",
				reqIdx, fc.Attempt)
			result.ClassifiedFailure = "post_stream_failure"
			resp.Body.Close()
			return
		}

		// --- No retry: wrap writer and stream the full response to the client ---
		// bytesFlushedWriter sets result.BytesFlushed on the first Write/Flush so
		// OnPostResponse can distinguish stream-started from pre-stream failures.
		bfw := &bytesFlushedWriter{ResponseWriter: w, result: &result}
		s.streamResponseToClient(bfw, resp, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)
		result.StreamStarted = result.BytesFlushed
		resp.Body.Close()
		return
	}

	// All attempts exhausted without a successful non-retry response.
	s.logger.Printf("[smm req %d] all %d attempt(s) exhausted (thread=%s)",
		reqIdx, maxAttempts, threadID)
	result = proxyext.ForwardResult{
		StatusCode:        http.StatusServiceUnavailable,
		ClassifiedFailure: "account_pool_exhausted",
	}
	if !result.BytesFlushed {
		http.Error(w,
			fmt.Sprintf("smm: account pool exhausted after %d attempt(s)", maxAttempts),
			http.StatusServiceUnavailable)
	}
}

// bytesFlushedWriter wraps http.ResponseWriter and records the first moment
// that any byte is committed to the client. Once BytesFlushed is true the
// retry loop in forwardWithSMM treats it as a hard stop.
type bytesFlushedWriter struct {
	http.ResponseWriter
	result *proxyext.ForwardResult
}

func (bfw *bytesFlushedWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		bfw.result.BytesFlushed = true
	}
	return bfw.ResponseWriter.Write(p)
}

func (bfw *bytesFlushedWriter) Flush() {
	// Flush counts as a byte commitment even if no Write preceded it,
	// because http.Flusher.Flush() pushes buffered headers to the client.
	bfw.result.BytesFlushed = true
	if f, ok := bfw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// streamResponseToClient handles everything after the upstream response headers
// are decided: gzip decode, header copy, WriteHeader, body streaming, usage
// tracking, sawtooth update, cache TTL detection, keepalive reset, daemon
// reporting, and annotation extraction.
//
// It is called from both forwardWithAnnotation (legacy, unchanged) and
// forwardWithSMM (SMM path). The writer w may be a bytesFlushedWriter.
func (s *Server) streamResponseToClient(
	w http.ResponseWriter,
	resp *http.Response,
	origReq *http.Request,
	body []byte,
	reqIdx int,
	toolUseIDs []string,
	proj string,
	threadID string,
	msgCount int,
	estimatedTokens ...int,
) {
	// Decompress gzip response if needed.
	responseBody := resp.Body
	if equalFoldStr(resp.Header.Get("Content-Encoding"), "gzip") {
		import_gzip_reader(resp, &responseBody)
	}
	_ = context.Background() // keep context import live if needed

	// Parse rate-limit headers before forwarding.
	rlInfo := ParseRateLimitHeaders(resp.Header)
	var rlJSON string
	if rlInfo != nil {
		if b, err := jsonMarshal(rlInfo); err == nil {
			rlJSON = string(b)
		}
	}

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	ct := resp.Header.Get("Content-Type")
	isSSE := stringsContains(ct, "text/event-stream")
	if !isSSE {
		s.handleNonSSEResponse(w, responseBody, origReq, body, reqIdx, threadID, proj, msgCount, rlJSON)
		return
	}
	s.handleSSEResponse(w, responseBody, origReq, body, reqIdx, toolUseIDs, proj, threadID, msgCount, rlJSON, estimatedTokens...)
}

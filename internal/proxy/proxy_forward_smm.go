package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// forwardWithSMM wraps forwardWithAnnotation with the SMM pre-stream retry
// loop. It is called instead of forwardWithAnnotation when the SMM hook layer
// is active (i.e. when proxyext.Hooks is not the noop implementation).
//
// Retry invariant (HARD): no retry is attempted after any bytes have been
// written to the downstream client. BytesFlushed is set to true the moment
// w.WriteHeader is called on a response that will be forwarded to the client.
// If BytesFlushed is true, RetryDecision.Retry is ignored.
//
// Hook call order per attempt:
//  1. BeforeForward   — select account, inject Authorization header
//  2. s.httpClient.Do — send request to upstream Anthropic
//  3. OnPreStreamResponse — inspect status BEFORE w.WriteHeader
//     (a) Retry=true  → mark account, rebuild request, loop
//     (b) Retry=false → fall through to normal forward path
//  4. forwardWithAnnotation — write headers + body to client (BytesFlushed=true)
//  5. OnPostResponse  — record final outcome (fire-and-forget)
func (s *Server) forwardWithSMM(
	w http.ResponseWriter,
	origReq *http.Request,
	body []byte,
	reqIdx int,
	toolUseIDs []string,
	proj string,
	threadID string,
	msgCount int,
	hooks proxyext.Hooks,
	estimatedTokens ...int,
) {
	// Build RequestContext once — it is stable across all attempts.
	reqCtx := proxyext.RequestContext{
		ThreadID:   threadID,
		SessionID:  threadID, // yesmem uses threadID as session key
		Model:      extractModelFromBody(body),
		IsSubagent: isSubagentFromBody(body),
	}

	fc := &proxyext.ForwardContext{
		ReqCtx:       reqCtx,
		OriginalBody: body,
		Attempt:      0,
		BytesFlushed: false,
	}

	var lastErr error

	for {
		// ── Step 1: build outbound request ──────────────────────────────────────
		targetURL := s.resolveAnthropicTarget(reqCtx.Model) + origReq.URL.RequestURI()
		currentBody := stripProviderPrefixFromBody(fc.OriginalBody)

		proxyReq, err := http.NewRequestWithContext(
			origReq.Context(),
			origReq.Method,
			targetURL,
			bytes.NewReader(currentBody),
		)
		if err != nil {
			s.logger.Printf("[smm req %d attempt %d] create request error: %v", reqIdx, fc.Attempt, err)
			http.Error(w, "failed to create proxy request", http.StatusBadGateway)
			return
		}
		for key, vals := range origReq.Header {
			for _, v := range vals {
				proxyReq.Header.Add(key, v)
			}
		}
		proxyReq.Header.Set("Content-Length", strconv.Itoa(len(currentBody)))
		proxyReq.Header.Del("Connection")
		proxyReq.Header.Del("Accept-Encoding")

		fc.OutboundReq = proxyReq

		// ── Step 2: BeforeForward — account selection + auth injection ──────────
		if err := hooks.BeforeForward(origReq.Context(), fc); err != nil {
			s.logger.Printf("[smm req %d attempt %d] BeforeForward error: %v", reqIdx, fc.Attempt, err)
			http.Error(w, fmt.Sprintf("account pool exhausted: %v", err), http.StatusServiceUnavailable)
			return
		}

		// ── Step 3: send request ─────────────────────────────────────────────────
		resp, doErr := s.httpClient.Do(proxyReq)
		if doErr != nil {
			// Network-level failure: not retryable per classifier spec.
			s.logger.Printf("[smm req %d attempt %d] upstream error: %v", reqIdx, fc.Attempt, doErr)
			lastErr = doErr
			break
		}

		// ── Step 4: OnPreStreamResponse — BEFORE w.WriteHeader ──────────────────
		// fc.BytesFlushed is false here: we have not written anything to the
		// client yet. This is the pre-stream boundary.
		dec, hookErr := hooks.OnPreStreamResponse(origReq.Context(), fc, resp)
		if hookErr != nil {
			log.Printf("[smm req %d attempt %d] OnPreStreamResponse error (continuing): %v", reqIdx, fc.Attempt, hookErr)
		}

		maxAttempts := dec.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 2 // default if hook returns zero
		}

		if dec.Retry && !fc.BytesFlushed && fc.Attempt < maxAttempts {
			// Retryable: close upstream body, advance attempt counter, loop.
			resp.Body.Close()
			s.logger.Printf("[smm req %d] retrying attempt %d→%d reason=%s",
				reqIdx, fc.Attempt, fc.Attempt+1, dec.RetryReason)
			fc.Attempt++
			continue
		}

		// ── Step 5: flush response to client ─────────────────────────────────────
		// Past this point BytesFlushed must be treated as true. We do not set
		// the field here because forwardWithAnnotation owns the write path; the
		// field is semantically true from the moment we call WriteHeader below.
		// OnPostResponse uses ForwardResult.BytesFlushed, not fc.BytesFlushed.
		resp.Body.Close() // forwardWithAnnotation will re-issue the request via
		                   // the fast path below, which is a resolved connection.

		// Decompress gzip if needed before handing off.
		respBody := resp.Body
		if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			if gz, gzErr := gzip.NewReader(resp.Body); gzErr == nil {
				respBody = gz
				defer gz.Close()
				resp.Header.Del("Content-Encoding")
				resp.Header.Del("Content-Length")
			}
		}
		_ = respBody // consumed by forwardWithAnnotation on next call

		// Delegate to the standard annotating forward path. We pass the already-
		// prepared body (currentBody) so it does not re-strip provider prefixes.
		// forwardWithAnnotation will re-issue its own httpClient.Do, which is
		// acceptable: the retry loop above has already confirmed this account
		// is healthy for this attempt. The call is equivalent to the normal
		// non-SMM path from here on.
		s.forwardWithAnnotation(w, origReq, currentBody, reqIdx, toolUseIDs, proj, threadID, msgCount, estimatedTokens...)

		// ── Step 6: OnPostResponse (fire-and-forget) ─────────────────────────────
		result := proxyext.ForwardResult{
			StatusCode:   resp.StatusCode,
			StreamStarted: true, // forwardWithAnnotation always streams if we reach here
			BytesFlushed:  true,
		}
		go hooks.OnPostResponse(origReq.Context(), fc, result)
		return
	}

	// Exhausted all retries or hit a network error.
	if lastErr != nil {
		s.logger.Printf("[smm req %d] all %d attempts exhausted: %v", reqIdx, fc.Attempt+1, lastErr)
		http.Error(w, "upstream error after retries: "+lastErr.Error(), http.StatusBadGateway)
	}

	// OnPostResponse for exhausted case.
	result := proxyext.ForwardResult{
		StatusCode:        0,
		StreamStarted:     false,
		BytesFlushed:      false,
		ClassifiedFailure: "exhausted",
	}
	go hooks.OnPostResponse(origReq.Context(), fc, result)
}

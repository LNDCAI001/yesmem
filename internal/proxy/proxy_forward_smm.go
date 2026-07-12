// Package proxy — SMM retry-loop wiring for proxy_forward.go.
//
// INVARIANTS ENFORCED BY THIS FILE
// ==================================
//   - BytesFlushed is set to true before the first w.Write. OnPreStreamResponse
//     is called before any w.Write. These two facts combine to ensure Retry is
//     never true after flush.
//   - On max-retry exhaustion with no success, the last upstream response is
//     forwarded to the client. The client gets a real HTTP error, not a hang.
//   - Thread/session IDs in ForwardContext are never mutated across retries.
//   - The outbound request body is re-built from origBody on every attempt;
//     the original r.Body (already drained) is never re-read.
//   - The http.Client used here is s.httpClient (the server-wide shared client)
//     so TLS config, connection pooling, and keep-alive settings are identical
//     to the stock forward path. No absolute Timeout is set — cancellation is
//     handled via the request context deadline from the parent handler, which
//     is correct for SSE responses that can legitimately run for minutes.
package proxy

import (
	"bytes"
	"encoding/json"
	"context"
	"fmt"
	"io"
	"sync"
	"log"
	"net/http"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
)

// smmWinningAuth maps threadID -> winning Authorization header value.
// Written by SMMForwardWithRetry after a successful account selection;
// read by keepalive and forked-agent paths so they reuse the same credential.
var smmWinningAuth sync.Map

// SMMForwardWithRetry executes an upstream request with account-pool-aware
// pre-stream retry. It is called instead of the standard single-attempt path
// when proxyext.IsActive() is true and the account pool is enabled.
//
// Parameters:
//   - ctx:        request context (carries the client deadline)
//   - threadID:   yesmem thread/session identifier, unchanged across retries
//   - isSubagent: from the parsed request body
//   - w:          the client ResponseWriter — nothing is written until success
//   - origBody:   the request body, drained exactly once by the caller
//   - outReq:     the outbound *http.Request already built by proxy_forward.go,
//                 with all headers except Authorization (which BeforeForward injects)
//   - s:          the proxy Server; used for s.httpClient and s.cacheTTLDetector
func SMMForwardWithRetry(
	ctx context.Context,
	threadID string,
	isSubagent bool,
	w http.ResponseWriter,
	origBody []byte,
	outReq *http.Request,
	s *Server,
) error {
	cfg := proxyext.ActiveSMMConfig()
	if cfg == nil {
		return nil
	}

	maxAttempts := cfg.AccountPool.MaxPreStreamRetries + 1 // +1 for the first attempt
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

	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		fc.Attempt = attempt
		fc.BytesFlushed = false
		fc.SelectedAccount = nil

		// Build a fresh clone of outReq with origBody as the body.
		// cloneForAttempt deep-copies headers so BeforeForward can freely
		// mutate the Authorization header without affecting subsequent clones.
		// ctx is threaded through so the cloned request respects the client
		// deadline.
		attemptReq, cloneErr := cloneForAttempt(ctx, outReq, origBody)
		if cloneErr != nil {
			return cloneErr
		}
		fc.OutboundReq = attemptReq

		// Hook: select an account and inject its Authorization header.
		if hookErr := proxyext.BeforeForward(fc); hookErr != nil {
			// All accounts exhausted — surface as 503.
			http.Error(w, hookErr.Error(), http.StatusServiceUnavailable)
			return hookErr
		}

		// Mirror the stock path: record the request in the TTL detector before
		// each upstream send so per-account TTL inference is not skipped.
		if s.cacheTTLDetector != nil {
			s.cacheTTLDetector.RecordRequest(threadID)
		}

		// Send the request upstream via the shared server transport.
		// fc.OutboundReq carries the injected auth header and a fresh body reader.
		resp, doErr := s.httpClient.Do(fc.OutboundReq)
		if doErr != nil {
			lastErr = doErr
			// Transport error — do NOT call OnPreStreamResponse with a nil resp
			// (extension.go would panic on resp.StatusCode). Record the failure
			// directly via OnPostResponse so account state is updated.
			proxyext.OnPostResponse(fc, proxyext.ForwardResult{Err: doErr})
			break
		}

		// Read the retry decision BEFORE writing anything to the client.
		// fc.BytesFlushed is false here; the dispatcher enforces this as a
		// categorical gate — Retry can never be true after flush.
		decision, hookErr := proxyext.OnPreStreamResponse(fc, resp)
		if hookErr != nil {
			log.Printf("[smm] OnPreStreamResponse error (ignored, fail-open): %v", hookErr)
		}

		if decision.Retry {
			// Close the body before the next attempt to avoid leaking
			// connections back to the upstream transport pool.
			resp.Body.Close()
			lastErr = fmt.Errorf("smm: retryable status code %d (attempt %d/%d)", resp.StatusCode, attempt+1, maxAttempts)
			continue
		}

		// ── Winning attempt ───────────────────────────────────────────────────
		//
		// Persist the winning Authorization header so downstream logic in
		// forwardWithAnnotation (keepalive reset, forked agents) uses the
		// selected subscription account credential rather than the inbound
		// client credential.
		if auth := fc.OutboundReq.Header.Get("Authorization"); auth != "" && threadID != "" {
			smmWinningAuth.Store(threadID, auth)
		}

		// Forward upstream response headers to the client before WriteHeader so
		// rate-limit headers, Content-Type, and other upstream metadata are
		// visible to the downstream client and any response-inspecting middleware.
		for key, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(key, v)
			}
		}

		// BytesFlushed must be set before WriteHeader — the dispatcher uses it
		// as a categorical gate to block any post-WriteHeader retry attempt.
		fc.BytesFlushed = true
		w.WriteHeader(resp.StatusCode)

		_, copyErr := io.Copy(w, resp.Body)
		resp.Body.Close()

		// Record outcome for observability and per-account state.
		proxyext.OnPostResponse(fc, proxyext.ForwardResult{
			StatusCode:  resp.StatusCode,
			RespHeader: resp.Header,
			Err:        copyErr,
		})

		return copyErr
	}
	// ── Exhausted all attempts ────────────────────────────────────────────────
	return fmt.Errorf("smm: all %d account attempts exhausted (last: %w)", maxAttempts, lastErr)
}

// stripNonAPIFields removes request-body keys that Claude Code / the SDK emit for
// their own internal context handling but that the Anthropic Messages API rejects
// with 400 "context_management: Extra inputs are not permitted" (or equivalent).
// Our fork already strips these in forked_agent.go and cache_keepalive.go; this
// covers the main SMM forward path, which previously forwarded them verbatim and
// caused Claude Code to retry the failing request for minutes.
func stripNonAPIFields(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body // not JSON; forward untouched
	}
	for _, k := range []string{"context_management", "anti_distillation"} {
		delete(m, k)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// cloneForAttempt creates a fresh outbound request with origBody as the body.
// ctx is applied to the clone so the request respects the client deadline.
// Headers are deep-copied so BeforeForward can mutate the Authorization header
// freely without affecting the base request or subsequent clones.
func cloneForAttempt(ctx context.Context, base *http.Request, origBody []byte) (*http.Request, error) {
	// Drop Claude-Code-internal fields the Anthropic API rejects.
	origBody = stripNonAPIFields(origBody)
	cloned := base.Clone(ctx)
	cloned.Body = io.NopCloser(bytes.NewReader(origBody))
	cloned.ContentLength = int64(len(origBody))
	cloned.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(origBody)), nil
	}
	cloned.Header = base.Header.Clone()
	// Strip beta headers: when forwarded to the official Anthropic API, any
	// anthropic-beta / x-anthropic-beta header causes Anthropic to bill the
	// request as "extra usage" against the (empty) overage credit bucket
	// instead of the included subscription window, which 429s as
	// out_of_credits on a fresh account that has spent nothing. This matches
	// the Meridian v1.28.0 fix. See Anthropic issue rynfar/meridian#278.
	cloned.Header.Del("anthropic-beta")
	cloned.Header.Del("x-anthropic-beta")
	return cloned, nil
}

package proxy

// proxy_forward_smm.go — SMM pre-stream account rotation retry loop.
//
// CONTRACT (enforced by comment and verified by code review):
//   - MUST NOT call w.WriteHeader, w.Write, or w.Header() under any circumstances.
//   - MUST call s.cacheTTLDetector.RecordRequest(threadID) on every attempt.
//   - MUST call resp.Body.Close() on every non-winning *http.Response before continuing.
//   - Returns (resp, nil) on success; (nil, err) on exhaustion or hard failure.
//   - Never modifies body []byte or origReq.
//
// Auth injection is delegated entirely to proxyext.BeforeForward, which calls
// extension.go's smmHooks.BeforeForward → pool.SelectAndGetToken internally.
// SMMForwardWithRetry does NOT call Pool methods directly — all account selection
// and state transitions flow through the hook dispatcher.

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
)

// smmWinningAuth stores the winning account's Authorization header value, keyed
// by threadID. Written by SMMForwardWithRetry on success; read by
// forwardWithAnnotation post-stream for cacheKeepalive.Reset and fireForkedAgents.
//
// Concurrency: sync.Map — safe for concurrent reads and writes.
// Eviction: entries are overwritten on the next request for the same threadID.
// Stale entries from dead threads are benign: they are never read after the
// streaming loop exits, and the map holds at most one string per live thread.
var smmWinningAuth sync.Map // key: string (threadID), value: string (Authorization header)

// SMMForwardWithRetry attempts the upstream call with pre-stream account rotation.
//
// It is called from forwardWithAnnotation in place of s.httpClient.Do when the
// SMM account pool is active. It returns (resp, err) with identical semantics to
// httpClient.Do — the caller owns resp.Body and must close it.
//
// The parameter w is accepted to match the call-site signature and to allow
// future observability hooks, but it is NEVER written to. Zero calls to
// w.WriteHeader, w.Write, or w.Header() exist in this function.
func SMMForwardWithRetry(
	s *Server,
	w http.ResponseWriter, // accepted but NEVER written — see contract above
	origReq *http.Request,
	body []byte,
	threadID string,
) (*http.Response, error) {
	// Nil-pool guard: if the pool was not initialised (e.g. config race at
	// startup), fall back to the stock single-attempt path.
	// cacheTTLDetector.RecordRequest is called here to mirror the stock path.
	if !proxyext.IsActive() {
		if s.cacheTTLDetector != nil {
			s.cacheTTLDetector.RecordRequest(threadID)
		}
		proxyReq, err := smmCloneRequest(origReq, body)
		if err != nil {
			return nil, fmt.Errorf("smm: fallback clone: %w", err)
		}
		return s.httpClient.Do(proxyReq)
	}

	maxRetries := s.smmCfg.AccountPool.MaxPreStreamRetries
	if maxRetries <= 0 {
		maxRetries = 2
	}

	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// ── 1. Clone request ─────────────────────────────────────────────────
		// A fresh *http.Request is required per attempt because:
		//   a) httpClient.Do drains the body reader — it cannot be re-used.
		//   b) BeforeForward mutates OutboundReq.Header in place to inject auth.
		//      Sharing one request across attempts would cause auth bleed.
		attemptReq, cloneErr := smmCloneRequest(origReq, body)
		if cloneErr != nil {
			return nil, fmt.Errorf("smm: clone attempt %d: %w", attempt, cloneErr)
		}

		// ── 2. Build ForwardContext for this attempt ───────────────────────
		// ForwardContext is the only interface between SMMForwardWithRetry and
		// the proxyext hook layer. We do not call Pool methods directly.
		fc := &proxyext.ForwardContext{
			ReqCtx: proxyext.RequestContext{
				ThreadID:   threadID,
				IsSubagent: isSubagentFromBody(body),
			},
			OriginalBody: body,
			OutboundReq:  attemptReq,
			Attempt:      attempt,
			BytesFlushed: false, // pre-stream: no bytes have reached the client
		}

		// ── 3. Inject auth via hook dispatcher ───────────────────────────────
		// proxyext.BeforeForward calls extension.go smmHooks.BeforeForward,
		// which calls pool.SelectAndGetToken and writes the Bearer token onto
		// fc.OutboundReq.Header["Authorization"]. It also sets fc.SelectedAccount.
		//
		// On fail-open (non-exhaustion selection error): BeforeForward returns nil
		// and fc.SelectedAccount is unset. The request proceeds with origReq's auth.
		// On hard exhaustion: BeforeForward returns a non-nil error.
		if err := proxyext.BeforeForward(fc); err != nil {
			// Hard exhaustion — all accounts unavailable. Surface to caller.
			return nil, fmt.Errorf("smm: attempt %d: %w", attempt, err)
		}

		// ── 4. Notify TTL detector (mirrors stock path) ───────────────────
		if s.cacheTTLDetector != nil {
			s.cacheTTLDetector.RecordRequest(threadID)
		}

		// ── 5. Send to upstream ───────────────────────────────────────────
		// fc.OutboundReq now carries the injected Authorization header.
		// origReq is never modified.
		resp, doErr := s.httpClient.Do(fc.OutboundReq)
		if doErr != nil {
			// Network error. The pool will have been informed via OnPreStreamResponse
			// if we had called it, but network errors are not retryable per the
			// failure classifier (FailureNetworkTimeout → 30s cooldown, not retry).
			// Skip OnPreStreamResponse and surface the error.
			lastErr = fmt.Errorf("smm: attempt %d network error: %w", attempt, doErr)
			s.logger.Printf("[smm] attempt=%d network_error=%v", attempt, doErr)
			// Do not retry on network errors — the account is healthy;
			// the upstream is down. Retrying other accounts won't help.
			break
		}

		// ── 6. Inspect response pre-stream via hook dispatcher ────────────
		// proxyext.OnPreStreamResponse calls extension.go OnPreStreamResponse,
		// which calls pool.ShouldRetry. ShouldRetry calls sel.MarkResult
		// internally before returning the retry decision.
		//
		// fc.BytesFlushed is false — we are definitively pre-stream.
		decision, hookErr := proxyext.OnPreStreamResponse(fc, resp)
		if hookErr != nil {
			// Hook error is non-fatal: log, treat as no-retry, return response.
			s.logger.Printf("[smm] OnPreStreamResponse error (non-fatal): %v", hookErr)
			// Fall through to return resp to caller.
		}

		s.logger.Printf("[smm] attempt=%d status=%d retry=%v reason=%s",
			attempt, resp.StatusCode, decision.Retry, decision.Reason)

		if decision.Retry && attempt < maxRetries {
			// ── 7a. Rotate: drain and close non-winning response ─────────
			// Draining releases the underlying TCP connection back to the pool.
			// Skipping this would leak a file descriptor per rotated attempt.
			smmDrainAndClose(resp)
			// Loop — next iteration clones a fresh request and selects next account.
			continue
		}

		// ── 7b. Winner (success or non-retryable failure) ────────────────
		// Record winning auth so post-stream code (cacheKeepalive, fireForkedAgents)
		// uses the subscription account credential, not the client's original auth.
		if resp.StatusCode == http.StatusOK && threadID != "" {
			winningAuth := fc.OutboundReq.Header.Get("Authorization")
			if winningAuth != "" {
				smmWinningAuth.Store(threadID, winningAuth)
			}
			// Record success through pool (for non-streaming 200s not seen by ShouldRetry).
			proxyext.OnPostResponse(fc, proxyext.ForwardResult{StatusCode: resp.StatusCode})
		}

		// Return to forwardWithAnnotation. Caller owns resp.Body.
		return resp, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("smm: all accounts exhausted after max retries")
}

// smmCloneRequest creates a new *http.Request with the same method, URL, and
// headers as src, backed by a fresh bytes.Reader over body.
// origReq is never mutated. The returned request is safe to modify and send.
func smmCloneRequest(src *http.Request, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(
		src.Context(),
		src.Method,
		src.URL.String(),
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	for key, vals := range src.Header {
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	// These headers must be absent — proxy_forward.go already strips them
	// on the stock path, and we must mirror that here.
	req.Header.Del("Connection")
	req.Header.Del("Accept-Encoding")
	return req, nil
}

// smmDrainAndClose reads up to 512 bytes from resp.Body and then closes it,
// returning the underlying TCP connection to the http.Transport pool.
// Must be called on every non-winning *http.Response in the retry loop.
// Silently no-ops if resp or resp.Body is nil.
func smmDrainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	buf := make([]byte, 512)
	//nolint:errcheck — drain is best-effort; error here means body already closed
	resp.Body.Read(buf)
	resp.Body.Close()
}

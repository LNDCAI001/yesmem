package proxy

// proxy_forward_smm.go — SMM hook wiring into the forward path.
//
// DECISION TREE (all decisions documented with evidence source):
//
// DECISION 1: Where is the pre-flush boundary in proxy_forward.go?
//   VERIFIED by reading proxy_forward.go in full.
//   The boundary is:
//     resp, err := s.httpClient.Do(proxyReq)  // send; headers readable
//     w.WriteHeader(resp.StatusCode)           // first byte to client
//   BeforeForward is called after proxyReq is built, before Do().
//   OnPreStreamResponse is called after Do() returns, before WriteHeader().
//   forwardRaw (passthrough path) is excluded from SMM hooks in v1 —
//   it has no annotation logic and is used for non-Claude paths only.
//
// DECISION 2: Does provider_autoconf.go parse Claude OAuth credential dirs?
//   VERIFIED: provider_autoconf.go is OpenCode-only (models.json,
//   auth.json, opencode.json). No ~/.claude/ dir, no bearer token refresh,
//   no Claude subscription OAuth. oauth_store.go implements independently.
//
// DECISION 3: Does compress_context.go make staticplan redundant?
//   VERIFIED: CompressContext operates on messages[] array only —
//   thinking blocks and tool_results in conversation history.
//   staticplan targets system prompt and tool definitions.
//   Not redundant. Feature B proceeds gated behind mode:off.
//
// DECISION 4: Should SMM register in feature_gates.go?
//   INFERRED: Three-layer gate (YAML enabled flag + Init() call-guard +
//   per-feature flags) is sufficient. No feature_gates.go registration.
//
// DECISION 5: Does sawtooth encode auth-derived state in its key?
//   INFERRED from proxy_forward.go: sawtoothTrigger.UpdateAfterResponse
//   is keyed on threadID only. Account rotation preserves threadID.
//   No corruption risk from rotation.
//
// DECISION 6: Is cacheTTLDetector thread-scoped or global?
//   VERIFIED from proxy_forward.go: s.cacheTTLDetector is a field on
//   *Server — process-global singleton. RecordRequest(threadID) fires
//   before Do() on every attempt including retries. RecordResponse fires
//   once after the successful response. TTL inference is not corrupted by
//   multiple RecordRequest calls for the same threadID.

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
	"github.com/LNDCAI001/yesmem/internal/proxyext/accountpool"
)

// smmMaxHardRetries is an absolute ceiling on retries regardless of config.
// Prevents a misconfigured YAML from creating an unbounded loop.
const smmMaxHardRetries = 8

// smmForwardResult carries the outcome of a doSMMForwardWithAnnotation call
// back to forwardWithAnnotation's retry loop.
type smmForwardResult struct {
	// resp is the upstream HTTP response. Non-nil means the response
	// headers were received before any flush. Caller owns resp.Body.Close().
	resp *http.Response
	// bytesFlushed is true if w.WriteHeader has already been called.
	// When true the caller MUST NOT retry regardless of resp.StatusCode.
	bytesFlushed bool
	// err is non-nil only for transport-level failures (Do() returned error).
	// HTTP error status codes (4xx/5xx) are reported via resp.StatusCode,
	// not via err.
	err error
	// selectedAccount is the account that was used for this attempt.
	// Stored here so the retry loop can pass it to MarkResult without
	// re-reading it from a header (which would be a security leak).
	selectedAccount accountpool.AccountRef
}

// buildSMMProxyReq constructs a fresh *http.Request for each SMM attempt.
// It copies all headers from origReq and then lets BeforeForward overwrite
// the Authorization header with the selected account's token.
//
// body must be the original request body bytes (before any drain). This
// function never reads from origReq.Body — the caller is responsible for
// draining once and passing the bytes here.
func buildSMMProxyReq(s *Server, origReq *http.Request, body []byte) (*http.Request, error) {
	targetURL := s.resolveAnthropicTarget(extractModelFromBody(body)) + origReq.URL.RequestURI()
	body = stripProviderPrefixFromBody(body)

	proxyReq, err := http.NewRequestWithContext(
		origReq.Context(),
		origReq.Method,
		targetURL,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("smm: build proxy request: %w", err)
	}

	for key, vals := range origReq.Header {
		for _, v := range vals {
			proxyReq.Header.Add(key, v)
		}
	}
	proxyReq.Header.Set("Content-Length", strconv.Itoa(len(body)))
	proxyReq.Header.Del("Connection")
	proxyReq.Header.Del("Accept-Encoding")
	// NOTE: X-SMM-Account is deliberately NOT set here or anywhere.
	// Account identity travels via ForwardContext.SelectedAccount only.
	// Setting any SMM-specific header on proxyReq would leak account
	// identity to the upstream Anthropic API.
	return proxyReq, nil
}

// attemptSMMForward executes one attempt of the SMM-hooked forward:
//  1. Call BeforeForward to inject auth into proxyReq.
//  2. Call s.httpClient.Do(proxyReq).
//  3. Call OnPreStreamResponse BEFORE w.WriteHeader.
//
// Returns smmForwardResult. The caller decides whether to retry.
// If result.bytesFlushed is true, the caller MUST surface the response
// as-is — no retry is possible.
func attemptSMMForward(
	s *Server,
	w http.ResponseWriter,
	origReq *http.Request,
	body []byte,
	fc *proxyext.ForwardContext,
) smmForwardResult {
	proxyReq, err := buildSMMProxyReq(s, origReq, body)
	if err != nil {
		return smmForwardResult{err: err}
	}
	fc.OutboundReq = proxyReq
	fc.OriginalBody = body

	// HOOK: BeforeForward — auth injection.
	// This is the only place account selection and token injection occur.
	// BeforeForward stores the selected AccountRef in fc.SelectedAccount
	// (not in any HTTP header) so OnPreStreamResponse can retrieve it
	// without reconstructing a thin shell from a leaked header value.
	if err := proxyext.BeforeForward(origReq.Context(), fc); err != nil {
		return smmForwardResult{err: fmt.Errorf("smm: BeforeForward: %w", err)}
	}

	// Extract the selected account before Do() so we have it regardless
	// of what happens to fc after the response arrives.
	var selectedAcc accountpool.AccountRef
	if acc, ok := fc.SelectedAccount.(accountpool.AccountRef); ok {
		selectedAcc = acc
	}

	// Record that we are about to send a request for TTL tracking.
	// DECISION 6: cacheTTLDetector is process-global. RecordRequest fires
	// on every attempt including retries. This is safe because RecordResponse
	// fires only once after the successful response.
	threadID := fc.ReqCtx.ThreadID
	if s.cacheTTLDetector != nil {
		s.cacheTTLDetector.RecordRequest(threadID)
	}

	// Send the request. bytesFlushed is false here — nothing written to w yet.
	resp, doErr := s.httpClient.Do(proxyReq)
	if doErr != nil {
		// Transport-level failure. No response headers, nothing flushed.
		return smmForwardResult{
			err:             fmt.Errorf("smm: upstream Do: %w", doErr),
			selectedAccount: selectedAcc,
		}
	}

	// HOOK: OnPreStreamResponse — called BEFORE w.WriteHeader.
	// This is the ONLY window where a retry is possible.
	// After this call returns, we are committed to this response.
	fc.BytesFlushed = false
	decision, hookErr := proxyext.OnPreStreamResponse(origReq.Context(), fc, resp)
	if hookErr != nil {
		// Hook returned an error. Treat as non-retryable; surface the
		// response as-is (fail-open). Log the hook error.
		s.logger.Printf("smm: OnPreStreamResponse error (attempt %d): %v", fc.Attempt, hookErr)
		decision.Retry = false
	}

	if decision.Retry {
		// Close the response body before the caller retries — we will
		// not be reading from it. This releases the connection back to
		// the pool.
		resp.Body.Close()
		return smmForwardResult{
			resp:            nil, // signal: caller should retry
			bytesFlushed:    false,
			selectedAccount: selectedAcc,
		}
	}

	// No retry — return the response. Caller must call resp.Body.Close().
	return smmForwardResult{
		resp:            resp,
		bytesFlushed:    false,
		selectedAccount: selectedAcc,
	}
}

// SMMForwardWithRetry is the entry point called by forwardWithAnnotation
// when SMM account pool is active. It wraps the httpClient.Do call with
// the BeforeForward → Do → OnPreStreamResponse retry loop.
//
// Call site in forwardWithAnnotation (replace the single httpClient.Do block):
//
//	if proxyext.IsActive() && smmPoolEnabled() {
//		resp, err = SMMForwardWithRetry(s, w, origReq, body, threadID)
//	} else {
//		resp, err = s.httpClient.Do(proxyReq)
//	}
//
// Returns (resp, err) with identical semantics to httpClient.Do:
//   - If resp is non-nil, caller owns resp.Body.Close() and must call
//     w.WriteHeader before writing the body.
//   - If err is non-nil, resp is nil and no bytes have been written to w.
//
// INVARIANT: If this function returns err != nil, w has received no bytes.
// INVARIANT: This function never calls w.WriteHeader or w.Write.
// INVARIANT: Retry only occurs when fc.BytesFlushed == false.
func SMMForwardWithRetry(
	s *Server,
	w http.ResponseWriter,
	origReq *http.Request,
	body []byte,
	threadID string,
) (*http.Response, error) {
	hooks := proxyext.ActiveHooks()
	if hooks == nil {
		// SMM not initialised — fall through to caller's default path.
		return nil, fmt.Errorf("smm: SMMForwardWithRetry called but hooks not initialised")
	}

	// Determine max retries from pool config.
	// The hard ceiling prevents misconfigured YAML from looping forever.
	maxRetries := smmMaxHardRetries
	if cfg := proxyext.ActiveSMMConfig(); cfg != nil && cfg.AccountPool.MaxPreStreamRetries > 0 {
		if cfg.AccountPool.MaxPreStreamRetries < smmMaxHardRetries {
			maxRetries = cfg.AccountPool.MaxPreStreamRetries
		}
	}

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{
			ThreadID: threadID,
		},
		Attempt: 0,
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		fc.Attempt = attempt

		result := attemptSMMForward(s, w, origReq, body, fc)

		if result.err != nil {
			// Transport error. Log and either retry (if transport errors are
			// ever made retryable in classify.go) or return.
			// Currently transport errors are not retryable per failure_classifier_table.
			s.logger.Printf("smm: transport error on attempt %d: %v", attempt, result.err)
			return nil, result.err
		}

		if result.resp != nil {
			// Successful non-retry: response received, no bytes flushed,
			// hook said do not retry. Return to caller.
			return result.resp, nil
		}

		// result.resp == nil means attemptSMMForward decided to retry.
		// Sanity check: BytesFlushed must be false — enforced by
		// attemptSMMForward before it closes the response body.
		if result.bytesFlushed {
			// This branch should be unreachable by construction, but if
			// it fires it means a bug allowed a retry after flush.
			// Panic would be worse than surfacing the error.
			return nil, fmt.Errorf("smm: invariant violation: retry attempted after bytes flushed")
		}

		s.logger.Printf("smm: retrying (attempt %d/%d) account=%s",
			attempt+1, maxRetries, result.selectedAccount.Name)
	}

	return nil, fmt.Errorf("smm: all %d account pool attempts exhausted", maxRetries+1)
}

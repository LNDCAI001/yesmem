package proxyext

import (
	"net/http"
	"sync"
)

// Hooks is the interface the SMM extension implements. All methods are called
// synchronously on the request goroutine. Implementations must be safe for
// concurrent use — multiple requests share a single Hooks instance.
//
// The dispatcher functions below (BeforeForward, OnPreStreamResponse, etc.)
// are the only intended callers. internal/proxy/* must never call Hooks
// methods directly.
type Hooks interface {
	// BeforeForward is called after the outbound request is built but before
	// httpClient.Do. It may mutate fc.OutboundReq headers (auth injection).
	// It must not write to the ResponseWriter.
	BeforeForward(fc *ForwardContext) error

	// OnPreStreamResponse is called after response headers arrive and before
	// w.WriteHeader. It must not write to the ResponseWriter.
	// fc.BytesFlushed is always false when this is called by the dispatcher.
	OnPreStreamResponse(fc *ForwardContext, resp *http.Response) (RetryDecision, error)

	// OnPostResponse is called after the response body has been fully written
	// to the client. It must not block the request goroutine for more than a
	// few microseconds — use a goroutine for slow work.
	OnPostResponse(fc *ForwardContext, result ForwardResult)

	// TransformStaticPayload may rewrite the assembled prompt before it is
	// serialised. Errors are swallowed by the dispatcher (fail-open).
	TransformStaticPayload(fc *ForwardContext, prompt *AssembledPrompt) error
}

// ── process-level singleton ──────────────────────────────────────────────────

var (
	hooksMu     sync.RWMutex
	activeHooks Hooks
	activeCfg   *SMMConfig
)

// Init sets the process-wide hook implementation and config. Called once at
// server startup from proxy.go. Calling Init again replaces the previous
// implementation (safe for tests via ResetHooksForTest; not recommended in
// production).
func Init(h Hooks, cfg *SMMConfig) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	activeHooks = h
	activeCfg = cfg
}

// ResetHooksForTest restores the singleton to its zero state. Call it in
// test cleanup (t.Cleanup(proxyext.ResetHooksForTest)) before using t.Parallel
// so that parallel tests do not race on the package-level variable.
func ResetHooksForTest() {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	activeHooks = nil
	activeCfg = nil
}

// IsActive returns true if a non-noop Hooks implementation is installed.
// Used by proxy_forward.go to gate the SMM code path with a single branch.
func IsActive() bool {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return activeHooks != nil && activeCfg != nil && activeCfg.Enabled
}

// ActiveHooks returns the installed Hooks implementation. Returns nil if Init
// has not been called. Callers must check for nil.
func ActiveHooks() Hooks {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return activeHooks
}

// ActiveSMMConfig returns the SMMConfig passed to Init. Returns nil if Init
// has not been called.
func ActiveSMMConfig() *SMMConfig {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return activeCfg
}

// ── dispatchers ──────────────────────────────────────────────────────────────
// These are the only functions internal/proxy/* should call. They guard nil,
// recover from panics in OnPostResponse, and enforce the BytesFlushed
// invariant on OnPreStreamResponse.

// BeforeForward dispatches to the active implementation.
// Returns nil immediately if no hooks are installed.
func BeforeForward(fc *ForwardContext) error {
	hooksMu.RLock()
	h := activeHooks
	hooksMu.RUnlock()
	if h == nil {
		return nil
	}
	return h.BeforeForward(fc)
}

// OnPreStreamResponse dispatches to the active implementation.
// If fc.BytesFlushed is true it short-circuits and returns Retry:false —
// this is the categorical gate that prevents post-flush retries regardless
// of what the implementation returns.
func OnPreStreamResponse(fc *ForwardContext, resp *http.Response) (RetryDecision, error) {
	if fc.BytesFlushed {
		return RetryDecision{Retry: false, Reason: "dispatcher_bytes_flushed"}, nil
	}
	hooksMu.RLock()
	h := activeHooks
	hooksMu.RUnlock()
	if h == nil {
		return RetryDecision{}, nil
	}
	return h.OnPreStreamResponse(fc, resp)
}

// OnPostResponse dispatches to the active implementation. Panics in the
// implementation are recovered and logged to prevent a misbehaving hook from
// crashing the server.
func OnPostResponse(fc *ForwardContext, result ForwardResult) {
	hooksMu.RLock()
	h := activeHooks
	hooksMu.RUnlock()
	if h == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// Log and continue — a panicking hook must not crash the server.
			_ = r
		}
	}()
	h.OnPostResponse(fc, result)
}

// TransformStaticPayload dispatches to the active implementation.
// Errors are swallowed (fail-open) — a broken transform must never drop the
// request.
func TransformStaticPayload(fc *ForwardContext, prompt *AssembledPrompt) error {
	hooksMu.RLock()
	h := activeHooks
	hooksMu.RUnlock()
	if h == nil {
		return nil
	}
	// Fail-open: swallow error — a broken staticplan must never drop a request.
	_ = h.TransformStaticPayload(fc, prompt)
	return nil
}

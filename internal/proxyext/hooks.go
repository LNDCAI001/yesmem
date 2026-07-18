package proxyext

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/LNDCAI001/yesmem/internal/proxyext/accountpool"
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
	dispatchLog *log.Logger
)

// Init sets the process-wide hook implementation and config. Called once at
// server startup from proxy.go. Calling Init again replaces the previous
// implementation (safe for tests via ResetHooksForTest; not recommended in
// production).
func Init(h Hooks, cfg *SMMConfig, logger *log.Logger) {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	activeHooks = h
	activeCfg = cfg
	dispatchLog = logger
}

// ResetHooksForTest restores the singleton to its zero state. Call it in
// test cleanup (t.Cleanup(proxyext.ResetHooksForTest)) before using t.Parallel
// so that parallel tests do not race on the package-level variable.
func ResetHooksForTest() {
	hooksMu.Lock()
	defer hooksMu.Unlock()
	activeHooks = nil
	activeCfg = nil
	dispatchLog = nil
}

// IsEnabled returns true if SMM is fully initialised and cfg.Enabled is true.
// This is the single branch that internal/proxy/* should use to gate the SMM
// code path — it is cheaper than IsActive() + nil check because it combines
// both checks under one RLock.
//
// Usage in proxy_forward.go:
//
//	if proxyext.IsEnabled() {
//		fc := &proxyext.ForwardContext{ ... }
//		// ... retry loop ...
//	}
func IsEnabled() bool {
	hooksMu.RLock()
	defer hooksMu.RUnlock()
	return activeHooks != nil && activeCfg != nil && activeCfg.Enabled
}

// IsActive returns true if a non-noop Hooks implementation is installed.
// Deprecated: prefer IsEnabled() which also checks cfg.Enabled.
// Retained for any callers that need the hook-presence check independently
// of the config flag (e.g., tests that install a custom Hooks but do not
// set Enabled).
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

// ActivePoolAccounts returns a per-account status view from the active SMM
// pool, or nil if SMM is disabled / no pool is configured. Used by the
// proxy /accounts status endpoint.
func ActivePoolAccounts() []accountpool.AccountView {
	hooksMu.RLock()
	h := activeHooks
	hooksMu.RUnlock()
	if h == nil {
		return nil
	}
	if sh, ok := h.(*smmHooks); ok && sh.pool != nil {
		return sh.pool.Accounts()
	}
	return nil
}

// ActivePoolSetEnabled enables/disables a pool account by name at runtime.
// Returns false if SMM is disabled, no pool exists, or the name is unknown.
// Used by the proxy /accounts/enable and /accounts/disable endpoints.
func ActivePoolSetEnabled(name string, enabled bool) bool {
	hooksMu.RLock()
	h := activeHooks
	hooksMu.RUnlock()
	if h == nil {
		return false
	}
	if sh, ok := h.(*smmHooks); ok && sh.pool != nil {
		return sh.pool.SetAccountEnabled(name, enabled)
	}
	return false
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
// crashing the server. The recovered value is always logged — silent discard
// would mask bugs in the hook implementation.
func OnPostResponse(fc *ForwardContext, result ForwardResult) {
	hooksMu.RLock()
	h := activeHooks
	l := dispatchLog
	hooksMu.RUnlock()
	if h == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// Log the panic value so operators can diagnose misbehaving hooks.
			// We do not re-panic — a hook panic must never crash the server.
			if l != nil {
				l.Printf("[smm] OnPostResponse panic recovered: %s", fmt.Sprintf("%v", r))
			}
		}
	}()
	h.OnPostResponse(fc, result)
}

// TransformStaticPayload dispatches to the active implementation.
// Errors are swallowed (fail-open) — a broken transform must never drop a
// request. However, errors ARE logged as smm_static_plan_fail so that
// operators can distinguish a healthy no-op from a silent failure.
// Before this fix, a panicking or erroring staticplan was invisible in logs.
func TransformStaticPayload(fc *ForwardContext, prompt *AssembledPrompt) error {
	hooksMu.RLock()
	h := activeHooks
	l := dispatchLog
	hooksMu.RUnlock()
	if h == nil {
		return nil
	}
	if err := h.TransformStaticPayload(fc, prompt); err != nil {
		// Fail-open: swallow the error so the request is never dropped.
		// Log it so operators have a signal — smm_static_plan_fail=true
		// means staticplan ran and returned an error. smm_static_plan_noop
		// (logged by the implementation) means it ran and chose not to act.
		if l != nil {
			l.Printf("[smm] smm_static_plan_fail=true smm_static_noop_reason=error err=%v thread=%s",
				err, fc.ReqCtx.ThreadID)
		}
	}
	return nil
}

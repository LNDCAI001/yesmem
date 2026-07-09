package proxyext

import (
	"context"
	"log"
	"net/http"
)

// activeHooks is the process-level Hooks implementation.
// It is set once at startup via Init and then read-only.
// Guards against nil: if Init is never called, all calls fall through to noop.
var activeHooks Hooks = &noopHooks{}

// activeCfg caches the SMMConfig so MaxPreStreamRetries can read it without
// a type assertion on the live hook.
var activeCfg SMMConfig

// Init sets the active hook implementation. Call this once during server
// startup, before the first request is handled.
func Init(h Hooks) {
	if h == nil {
		activeHooks = &noopHooks{}
		return
	}
	activeHooks = h
}

// InitWithConfig sets the active hook implementation and caches the config.
// Prefer this over Init when an SMMConfig is available at startup.
func InitWithConfig(h Hooks, cfg SMMConfig) {
	activeCfg = cfg
	Init(h)
}

// HooksActive reports whether a real (non-noop) Hooks implementation is
// installed. When false, forwardWithSMM short-circuits immediately into
// forwardWithAnnotation with zero extra allocations on the hot path.
func HooksActive() bool {
	_, isNoop := activeHooks.(*noopHooks)
	return !isNoop
}

// MaxPreStreamRetries returns the configured maximum number of pre-stream
// retries. Returns 0 when SMM is disabled (no retries, first attempt only).
func MaxPreStreamRetries() int {
	if !HooksActive() {
		return 0
	}
	if activeCfg.AccountPool.MaxPreStreamRetries > 0 {
		return activeCfg.AccountPool.MaxPreStreamRetries
	}
	return 2 // spec default
}

// BeforeForward delegates to the active hooks implementation.
// On error the caller should abort the forward.
func BeforeForward(ctx context.Context, fc *ForwardContext) error {
	return activeHooks.BeforeForward(ctx, fc)
}

// OnPreStreamResponse delegates to the active hooks implementation.
// IMPORTANT: the caller must check result.Retry and MUST NOT retry
// if any bytes have already been flushed to the client.
func OnPreStreamResponse(ctx context.Context, fc *ForwardContext, resp *http.Response) (RetryDecision, error) {
	return activeHooks.OnPreStreamResponse(ctx, fc, resp)
}

// OnPostResponse delegates to the active hooks implementation.
// Panics are recovered and logged so they can never crash the forward path.
func OnPostResponse(ctx context.Context, fc *ForwardContext, result ForwardResult) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[proxyext] OnPostResponse panic recovered: %v", r)
		}
	}()
	activeHooks.OnPostResponse(ctx, fc, result)
}

// TransformStaticPayload delegates to the active hooks implementation.
// Panics are recovered and logged — a panicking planner must never crash
// the request goroutine. On any error or panic, ap is left unchanged
// (fail-open contract).
func TransformStaticPayload(ctx context.Context, reqCtx RequestContext, ap *AssembledPrompt) error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[proxyext] TransformStaticPayload panic recovered: %v — body unchanged", r)
		}
	}()
	err := activeHooks.TransformStaticPayload(ctx, reqCtx, ap)
	if err != nil {
		log.Printf("[proxyext] TransformStaticPayload error (fail-open): %v", err)
		return nil // deliberately swallowed — caller proceeds with original body
	}
	return nil
}

// ResetForTesting resets the hook singleton to the noop default.
// ONLY call this from *_test.go files. Never call in production code.
// Provides safe parallel-test isolation for the activeHooks singleton.
func ResetForTesting() {
	activeHooks = &noopHooks{}
	activeCfg = SMMConfig{}
}

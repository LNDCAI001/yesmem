package proxyext

import (
	"context"
	"log"
	"net/http"
)

// ActiveHooks is the process-level Hooks implementation.
// It is set once at startup via Init and then read-only.
// Guards against nil: if Init is never called, all calls fall through to noop.
var activeHooks Hooks = &noopHooks{}

// Init sets the active hook implementation. Call this once during server
// startup, before the first request is handled.
func Init(h Hooks) {
	if h == nil {
		activeHooks = &noopHooks{}
		return
	}
	activeHooks = h
}

// HooksActive reports whether a real (non-noop) Hooks implementation is
// installed. When false, smmForwardWithRetry short-circuits immediately and
// the stock forwardWithAnnotation path is taken with zero extra allocations.
func HooksActive() bool {
	_, isNoop := activeHooks.(*noopHooks)
	return !isNoop
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
// On error the ap is left unchanged (fail-open contract).
func TransformStaticPayload(ctx context.Context, reqCtx RequestContext, ap *AssembledPrompt) error {
	err := activeHooks.TransformStaticPayload(ctx, reqCtx, ap)
	if err != nil {
		log.Printf("[proxyext] TransformStaticPayload error (fail-open): %v", err)
		return nil // deliberately swallowed — caller proceeds with original body
	}
	return nil
}

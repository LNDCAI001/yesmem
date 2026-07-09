// Package proxyext provides a thin extension surface over yesmem's proxy pipeline.
// All hooks default to noop behaviour so upstream yesmem semantics are fully
// preserved when no features are enabled.
//
// Hook insertion points in proxy_forward.go:
//
//	1. BeforeForward       — after proxyReq is built, before httpClient.Do
//	2. OnPreStreamResponse — after Do returns, BEFORE w.WriteHeader (retry window)
//	3. OnPostResponse      — after streaming completes or errors
//	4. TransformStaticPayload — at prompt assembly, before outbound serialisation
//
// THE CRITICAL INVARIANT:
//   OnPreStreamResponse must only return RetryDecision{Retry:true} when
//   resp has been received but w.WriteHeader has NOT yet been called.
//   Once WriteHeader fires the status is committed and retry is impossible.
package proxyext

import (
	"context"
	"net/http"
)

// active is the live implementation. Swap via Init() at startup.
// Defaults to noopImpl so the binary is safe with zero config.
var active Extension = &noopImpl{}

// Extension is the full hook interface. Implement all methods.
// Use noopImpl as your embed base to stay forward-compatible.
type Extension interface {
	// BeforeForward is called after the outbound request is built but before
	// it is sent. Use this to inject the correct auth header for the selected account.
	// Must not mutate the request body structure.
	BeforeForward(ctx context.Context, fc *ForwardContext) error

	// OnPreStreamResponse is called after the upstream response arrives but
	// BEFORE w.WriteHeader is called on the client response.
	// This is the ONLY safe window for pre-stream account rotation retry.
	// Return RetryDecision{Retry:true} only here — never after WriteHeader.
	OnPreStreamResponse(ctx context.Context, fc *ForwardContext, resp *http.Response) (RetryDecision, error)

	// OnPostResponse is called after streaming completes (success or failure).
	// Use this to record account health observations.
	OnPostResponse(ctx context.Context, fc *ForwardContext, result ForwardResult)

	// TransformStaticPayload is called at prompt assembly time.
	// It may normalise or restructure static content blocks in-place.
	// Must be deterministic. Must fail open (leave prompt unchanged on error).
	// Must not run on subagents unless explicitly configured.
	TransformStaticPayload(ctx context.Context, reqCtx RequestContext, assembled *AssembledPrompt) error
}

// Init replaces the active extension implementation.
// Call once at startup after config is loaded.
func Init(impl Extension) {
	if impl == nil {
		active = &noopImpl{}
		return
	}
	active = impl
}

// BeforeForward delegates to the active extension.
func BeforeForward(ctx context.Context, fc *ForwardContext) error {
	return active.BeforeForward(ctx, fc)
}

// OnPreStreamResponse delegates to the active extension.
func OnPreStreamResponse(ctx context.Context, fc *ForwardContext, resp *http.Response) (RetryDecision, error) {
	return active.OnPreStreamResponse(ctx, fc, resp)
}

// OnPostResponse delegates to the active extension.
func OnPostResponse(ctx context.Context, fc *ForwardContext, result ForwardResult) {
	active.OnPostResponse(ctx, fc, result)
}

// TransformStaticPayload delegates to the active extension.
func TransformStaticPayload(ctx context.Context, reqCtx RequestContext, assembled *AssembledPrompt) error {
	return active.TransformStaticPayload(ctx, reqCtx, assembled)
}

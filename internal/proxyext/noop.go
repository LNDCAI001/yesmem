package proxyext

import (
	"context"
	"net/http"
)

// noopHooks implements Hooks with zero behaviour.
// It is the default when no extension config is provided and is also
// useful in tests to verify stock yesmem-equivalent paths.
type noopHooks struct{}

// DefaultHooks returns the noop implementation.
// Callers should store this as a Hooks interface value.
func DefaultHooks() Hooks {
	return &noopHooks{}
}

func (n *noopHooks) BeforeForward(_ context.Context, _ *ForwardContext) error {
	return nil
}

func (n *noopHooks) OnPreStreamResponse(_ context.Context, _ *ForwardContext, _ *http.Response) (RetryDecision, error) {
	return RetryDecision{Retry: false}, nil
}

func (n *noopHooks) OnPostResponse(_ context.Context, _ *ForwardContext, _ ForwardResult) {}

func (n *noopHooks) TransformStaticPayload(_ context.Context, _ RequestContext, _ *AssembledPrompt) error {
	return nil
}

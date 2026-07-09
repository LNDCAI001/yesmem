package proxyext

import (
	"context"
	"net/http"
)

// noopImpl is the default Extension implementation.
// Every method is a no-op that preserves stock yesmem behaviour exactly.
// This is the regression baseline: with noopImpl active, SMM must behave
// identically to upstream yesmem on every request path.
type noopImpl struct{}

func (n *noopImpl) BeforeForward(_ context.Context, _ *ForwardContext) error {
	return nil
}

func (n *noopImpl) OnPreStreamResponse(_ context.Context, _ *ForwardContext, _ *http.Response) (RetryDecision, error) {
	return RetryDecision{Retry: false}, nil
}

func (n *noopImpl) OnPostResponse(_ context.Context, _ *ForwardContext, _ ForwardResult) {}

func (n *noopImpl) TransformStaticPayload(_ context.Context, _ RequestContext, _ *AssembledPrompt) error {
	return nil
}

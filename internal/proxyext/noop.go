package proxyext

import "net/http"

// noopHooks implements Hooks with zero behaviour.
// It is the default when SMM is disabled and is used in tests to verify
// stock yesmem-equivalent paths. All methods must compile against the Hooks
// interface defined in hooks.go — any signature mismatch is a build error.
type noopHooks struct{}

// DefaultHooks returns the noop implementation.
func DefaultHooks() Hooks {
	return &noopHooks{}
}

func (n *noopHooks) BeforeForward(_ *ForwardContext) error {
	return nil
}

func (n *noopHooks) OnPreStreamResponse(_ *ForwardContext, _ *http.Response) (RetryDecision, error) {
	return RetryDecision{Retry: false}, nil
}

func (n *noopHooks) OnPostResponse(_ *ForwardContext, _ ForwardResult) {}

func (n *noopHooks) TransformStaticPayload(_ *ForwardContext, _ *AssembledPrompt) error {
	return nil
}

package extraction

import "github.com/LNDCAI001/yesmem/internal/sanitize"

var _ LLMClient = (*SanitizingClient)(nil)

// SanitizingClient wraps an LLMClient and runs all string inputs through
// a Sanitizer before sending, and all string outputs through the Sanitizer
// after receiving. Schemas (for CompleteJSON) are structural and are passed
// through unchanged. Name() and Model() are pass-throughs.
type SanitizingClient struct {
	inner LLMClient
	san   sanitize.Sanitizer
}

// NewSanitizingClient wraps inner with a Sanitizer that runs before forwarding
// each prompt and after receiving each completion. san must not be nil; wrapping
// is explicit at the call site.
//
// Decorator order convention. SanitizingClient must be the OUTERMOST decorator
// in the LLMClient stack. The full chain reads inner-to-outer:
//
//   raw client  ->  BudgetClient  ->  GatedClient  ->  SanitizingClient
//
// Reasons to keep SanitizingClient outermost:
//
//  1. SetMaxBudgetPerCall in internal/extraction/cli_client.go uses a type
//     switch that recurses through GatedClient and BudgetClient to reach
//     CLIClient. The switch has no SanitizingClient case; if SanitizingClient
//     were the receiver, the switch falls through and the per-call budget cap
//     is silently not applied. Construction sites call SetMaxBudgetPerCall on
//     the inner handle BEFORE applying the sanitization wrap, preserving the
//     recursion path.
//  2. CheckBudget and HasBudget in internal/extraction/budget.go use
//     type-assertions that expect the chain GatedClient->BudgetClient. A
//     SanitizingClient interposed between them breaks those assertions and
//     causes the budget check to return unlimited.
//  3. GatedClient and BudgetClient meter the prompt size that reaches the
//     application boundary, so accounting reflects actual API cost. Metering
//     on post-redaction text would under-count rate and cost because
//     redaction shrinks payloads.
//  4. Outputs from the inner stack pass through the Sanitizer before any
//     caller-side logging or persistence; placing SanitizingClient inside
//     another wrapper would let raw secrets escape into instrumentation.
//
// Construction sites apply this convention through the
// wrapWithSanitizationIfEnabled helper in internal/daemon/daemon.go, which
// returns the inner client unchanged when secrets_sanitization.enabled is
// false. Direct calls to NewSanitizingClient exist where wrapping is
// unconditional: the evoClient, summarizeClient, extractClient, and
// briefingClient wires in daemon.go, and the client/qualityClient wires in
// internal/daemon/quickstart.go.
func NewSanitizingClient(inner LLMClient, san sanitize.Sanitizer) *SanitizingClient {
	return &SanitizingClient{inner: inner, san: san}
}

func (c *SanitizingClient) Complete(system, userMsg string, opts ...CallOption) (string, error) {
	system = c.san.Sanitize(system)
	userMsg = c.san.Sanitize(userMsg)
	out, err := c.inner.Complete(system, userMsg, opts...)
	return c.san.Sanitize(out), err
}

func (c *SanitizingClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...CallOption) (string, error) {
	system = c.san.Sanitize(system)
	userMsg = c.san.Sanitize(userMsg)
	out, err := c.inner.CompleteJSON(system, userMsg, schema, opts...)
	return c.san.Sanitize(out), err
}

func (c *SanitizingClient) Name() string  { return c.inner.Name() }
func (c *SanitizingClient) Model() string { return c.inner.Model() }

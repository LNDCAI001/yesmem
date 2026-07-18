package daemon

import (
	"strings"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/config"
	"github.com/LNDCAI001/yesmem/internal/extraction"
	"github.com/LNDCAI001/yesmem/internal/sanitize"
)

// spySummarizeClient is a minimal extraction.LLMClient implementation for wrap tests.
type spySummarizeClient struct {
	completeFunc func(system, userMsg string, opts ...extraction.CallOption) (string, error)
}

func (m *spySummarizeClient) Complete(system, userMsg string, opts ...extraction.CallOption) (string, error) {
	if m.completeFunc != nil {
		return m.completeFunc(system, userMsg, opts...)
	}
	return "", nil
}

func (m *spySummarizeClient) CompleteJSON(system, userMsg string, schema map[string]any, opts ...extraction.CallOption) (string, error) {
	return "{}", nil
}

func (m *spySummarizeClient) Name() string  { return "spy" }
func (m *spySummarizeClient) Model() string { return "spy-model" }

func TestSummarizeClient_WrappedAtAssignmentWhenSanitizationEnabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.SecretsSanitization.Enabled = true

	secret := "sk-ant-api03-AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555FFFF6666"
	mock := &spySummarizeClient{
		completeFunc: func(system, userMsg string, opts ...extraction.CallOption) (string, error) {
			return "leaked " + secret + " leaked", nil
		},
	}

	wrapped := wrapWithSanitizationIfEnabled(mock, cfg, sanitize.NewSecretRedactor(nil))
	out, err := wrapped.Complete("sys", "user")
	if err != nil {
		t.Fatalf("Complete err: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("expected redaction, got %q", out)
	}
}

func TestSummarizeClient_NotWrappedWhenSanitizationDisabled(t *testing.T) {
	cfg := &config.Config{}
	cfg.SecretsSanitization.Enabled = false

	mock := &spySummarizeClient{}
	wrapped := wrapWithSanitizationIfEnabled(mock, cfg, sanitize.NewSecretRedactor(nil))
	if wrapped != mock {
		t.Fatalf("expected raw mock returned when disabled, got %T", wrapped)
	}
}

func TestSummarizeClient_NilClientReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	cfg.SecretsSanitization.Enabled = true
	got := wrapWithSanitizationIfEnabled(nil, cfg, sanitize.NewSecretRedactor(nil))
	if got != nil {
		t.Fatalf("expected nil for nil client, got %T", got)
	}
}

func TestSummarizeClient_NilCfgReturnsClientUnchanged(t *testing.T) {
	mock := &spySummarizeClient{}
	got := wrapWithSanitizationIfEnabled(mock, nil, sanitize.NewSecretRedactor(nil))
	if got != mock {
		t.Fatalf("expected raw mock returned when cfg is nil, got %T", got)
	}
}

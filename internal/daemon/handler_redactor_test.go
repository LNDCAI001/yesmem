package daemon

import (
	"strings"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/sanitize"
)

func TestHandler_RedactorWiringPassthrough(t *testing.T) {
	h := &Handler{}
	in := "sk-ant-api03-foobarbazquxquuxcorgegraultplughfoobarbazquxquuxcorge"
	if got := h.redact(in); got != in {
		t.Fatalf("expected passthrough when redactor nil, got %q", got)
	}
}

func TestHandler_RedactorAppliedWhenSet(t *testing.T) {
	h := &Handler{redactor: sanitize.NewSecretRedactor(nil)}
	got := h.redact("token sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa here")
	if strings.Contains(got, "sk-ant-api03") {
		t.Fatalf("expected redaction, got %q", got)
	}
	if !strings.Contains(got, "[REDACTED:anthropic_api_key]") {
		t.Fatalf("expected marker, got %q", got)
	}
}

package proxyext_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
)

// TestNoopHooksNeverError verifies that the default noop implementation
// returns nil / no-retry for every hook call.
func TestNoopHooksNeverError(t *testing.T) {
	h := proxyext.DefaultHooks()

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{ThreadID: "t1"},
		OutboundReq: httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil),
	}

	if err := h.BeforeForward(fc); err != nil {
		t.Fatalf("BeforeForward: expected nil, got %v", err)
	}

	resp := &http.Response{StatusCode: 200}
	dec, err := h.OnPreStreamResponse(fc, resp)
	if err != nil {
		t.Fatalf("OnPreStreamResponse: expected nil error, got %v", err)
	}
	if dec.Retry {
		t.Fatal("OnPreStreamResponse: noop should never request retry")
	}

	h.OnPostResponse(fc, proxyext.ForwardResult{StatusCode: 200})

	ap := &proxyext.AssembledPrompt{System: []byte(`{"model":"claude-3-5-sonnet-20241022"}`)}
	if err := h.TransformStaticPayload(fc, ap); err != nil {
		t.Fatalf("TransformStaticPayload: expected nil, got %v", err)
	}
}

// TestNoopHooksPreserveBody verifies that the noop implementation never
// mutates RawBody.
func TestNoopHooksPreserveBody(t *testing.T) {
	h := proxyext.DefaultHooks()
	original := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	ap := &proxyext.AssembledPrompt{System: make([]byte, len(original))}
	copy(ap.System, original)

		_ = h.TransformStaticPayload(&proxyext.ForwardContext{}, ap)

	if string(ap.System) != string(original) {
		t.Fatalf("noop mutated body: got %q, want %q", ap.System, original)
	}
}

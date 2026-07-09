package proxyext_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// TestNoopHooksNeverError verifies that the default noop implementation
// returns nil / no-retry for every hook call.
func TestNoopHooksNeverError(t *testing.T) {
	h := proxyext.DefaultHooks()
	ctx := context.Background()

	fc := &proxyext.ForwardContext{
		ReqCtx: proxyext.RequestContext{ThreadID: "t1"},
		OutboundReq: httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil),
	}

	if err := h.BeforeForward(ctx, fc); err != nil {
		t.Fatalf("BeforeForward: expected nil, got %v", err)
	}

	resp := &http.Response{StatusCode: 200}
	dec, err := h.OnPreStreamResponse(ctx, fc, resp)
	if err != nil {
		t.Fatalf("OnPreStreamResponse: expected nil error, got %v", err)
	}
	if dec.Retry {
		t.Fatal("OnPreStreamResponse: noop should never request retry")
	}

	h.OnPostResponse(ctx, fc, proxyext.ForwardResult{StatusCode: 200})

	ap := &proxyext.AssembledPrompt{RawBody: []byte(`{"model":"claude-3-5-sonnet-20241022"}`)}
	if err := h.TransformStaticPayload(ctx, fc.ReqCtx, ap); err != nil {
		t.Fatalf("TransformStaticPayload: expected nil, got %v", err)
	}
	if ap.TransformApplied != "" {
		t.Fatal("noop should not set TransformApplied")
	}
}

// TestNoopHooksPreserveBody verifies that the noop implementation never
// mutates RawBody.
func TestNoopHooksPreserveBody(t *testing.T) {
	h := proxyext.DefaultHooks()
	original := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	ap := &proxyext.AssembledPrompt{RawBody: make([]byte, len(original))}
	copy(ap.RawBody, original)

	_ = h.TransformStaticPayload(context.Background(), proxyext.RequestContext{}, ap)

	if string(ap.RawBody) != string(original) {
		t.Fatalf("noop mutated body: got %q, want %q", ap.RawBody, original)
	}
}

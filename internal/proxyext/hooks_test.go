package proxyext_test

import (
	"net/http"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/proxyext"
)

func TestDefaultHooks_compile(t *testing.T) {
	t.Parallel()
	// Verifies that noopHooks compiles against the Hooks interface.
	// This test fails to compile if noop.go has wrong method signatures.
	var _ proxyext.Hooks = proxyext.DefaultHooks()
}

func TestDefaultHooks_noop(t *testing.T) {
	t.Parallel()
	h := proxyext.DefaultHooks()
	fc := &proxyext.ForwardContext{}

	if err := h.BeforeForward(fc); err != nil {
		t.Errorf("BeforeForward noop: %v", err)
	}

	dec, err := h.OnPreStreamResponse(fc, &http.Response{StatusCode: 200})
	if err != nil || dec.Retry {
		t.Errorf("OnPreStreamResponse noop: retry=%v err=%v", dec.Retry, err)
	}

	// OnPostResponse must not panic.
	h.OnPostResponse(fc, proxyext.ForwardResult{})

	// TransformStaticPayload must return nil.
	if err := h.TransformStaticPayload(fc, &proxyext.AssembledPrompt{}); err != nil {
		t.Errorf("TransformStaticPayload noop: %v", err)
	}
}

func TestDispatcher_bytesFlushed_gate(t *testing.T) {
	t.Parallel()
	// Even if a real implementation were installed, BytesFlushed=true must
	// produce Retry:false from the dispatcher. This test runs without Init,
	// exercising the dispatcher's own short-circuit before the nil check.
	fc := &proxyext.ForwardContext{BytesFlushed: true}
	dec, err := proxyext.OnPreStreamResponse(fc, &http.Response{StatusCode: 429})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if dec.Retry {
		t.Error("dispatcher must return Retry:false when BytesFlushed is true")
	}
	if dec.Reason != "dispatcher_bytes_flushed" {
		t.Errorf("expected reason=dispatcher_bytes_flushed, got %q", dec.Reason)
	}
}

func TestInit_and_IsEnabled(t *testing.T) {
	// NOT t.Parallel() — mutates the package-level singleton.
	t.Cleanup(proxyext.ResetHooksForTest)

	if proxyext.IsEnabled() {
		t.Fatal("IsEnabled should be false before Init")
	}

	cfg := &proxyext.SMMConfig{Enabled: true}
	proxyext.Init(proxyext.DefaultHooks(), cfg, nil)

	if !proxyext.IsEnabled() {
		t.Fatal("IsEnabled should be true after Init with Enabled:true")
	}
}

func TestInit_disabledConfig(t *testing.T) {
	// NOT t.Parallel() — mutates the package-level singleton.
	t.Cleanup(proxyext.ResetHooksForTest)

	cfg := &proxyext.SMMConfig{Enabled: false}
	proxyext.Init(proxyext.DefaultHooks(), cfg, nil)

	if proxyext.IsEnabled() {
		t.Error("IsEnabled should be false when cfg.Enabled is false")
	}
}

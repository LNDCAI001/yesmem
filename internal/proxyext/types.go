// Package proxyext is the SMM extension layer for yesmem.
// It provides a narrow hook surface that sits on top of yesmem's existing proxy
// without modifying its core logic. All hooks are noop by default.
package proxyext

import (
	"context"
	"net/http"

	"github.com/carsteneu/yesmem/internal/proxyext/accountpool"
)

// RequestContext carries per-request metadata threaded through hooks.
type RequestContext struct {
	ThreadID    string
	SessionID   string
	Model       string
	IsSubagent  bool
	Provider    string
	SourceAgent string
}

// ForwardContext is passed to BeforeForward and response hooks.
type ForwardContext struct {
	ReqCtx       RequestContext
	OriginalBody []byte
	OutboundReq  *http.Request
	// Attempt is 0 for the first try, 1 for the first retry, etc.
	Attempt int
	// SelectedAccount is set by BeforeForward and read by OnPreStreamResponse
	// and OnPostResponse. Carrying it here avoids leaking account identity into
	// any header that would reach the upstream API.
	SelectedAccount accountpool.AccountRef
}

// RetryDecision is returned by OnPreStreamResponse.
type RetryDecision struct {
	Retry       bool
	RetryReason string
	MaxAttempts int
}

// ForwardResult describes the outcome of a forwarded request.
type ForwardResult struct {
	StatusCode        int
	StreamStarted     bool
	// BytesFlushed is true when at least one byte has been written to the
	// downstream client. The retry loop in proxy_forward_smm.go treats this
	// as a hard stop: once BytesFlushed is true, no retry is possible
	// regardless of the upstream status code.
	BytesFlushed      bool
	ClassifiedFailure string
}

// AssembledPrompt is a mutable view of the prompt payload before it is sent.
// TransformStaticPayload may normalise or annotate fields but must not
// change semantics or remove content.
type AssembledPrompt struct {
	// RawBody is the JSON body bytes. Mutate this field in place if a
	// transform needs to rewrite the body; leave nil to skip re-encoding.
	RawBody []byte
	// ContentHash is computed by the caller; set by staticplan on output.
	ContentHash string
	// TransformApplied is set to a non-empty string by staticplan when it
	// successfully applies a transform.
	TransformApplied string
}

// Hooks is the interface satisfied by both noopHooks and real extension
// implementations. It is intentionally small.
type Hooks interface {
	// BeforeForward is called after the outbound request is built but before
	// it is sent. The implementation may inject the auth header for the
	// selected account. Returning a non-nil error aborts the forward.
	BeforeForward(ctx context.Context, fc *ForwardContext) error

	// OnPreStreamResponse is called after receiving the upstream response
	// headers but before any body bytes are flushed to the client.
	// If RetryDecision.Retry is true the caller MUST rebuild the request
	// from fc.OriginalBody and retry. The caller MUST NOT retry if any
	// bytes have already been written to the client.
	OnPreStreamResponse(ctx context.Context, fc *ForwardContext, resp *http.Response) (RetryDecision, error)

	// OnPostResponse is called after the response has been fully processed
	// (stream ended or non-stream body consumed). It is fire-and-forget;
	// errors are logged internally and never surfaced to the caller.
	OnPostResponse(ctx context.Context, fc *ForwardContext, result ForwardResult)

	// TransformStaticPayload may normalise or annotate the assembled prompt
	// before it is forwarded. It MUST be deterministic and MUST fail open
	// (leave ap unchanged if anything goes wrong).
	TransformStaticPayload(ctx context.Context, reqCtx RequestContext, ap *AssembledPrompt) error
}

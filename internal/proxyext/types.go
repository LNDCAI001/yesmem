package proxyext

import (
	"net/http"
)

// Hooks is the extension surface exposed to SMM features.
// Every method has a corresponding no-op implementation in noop.go.
type Hooks interface {
	// BeforeForward is called after the outbound request is fully constructed
	// but before it is sent to the upstream API. Implementations may inject
	// auth headers, select an account, or mutate fc. Returning a non-nil
	// error causes the request to fail immediately.
	BeforeForward(fc *ForwardContext) error

	// OnPreStreamResponse is called after the upstream response headers are
	// received but before any bytes are written to the client. This is the
	// only safe retry window. Once fc.BytesFlushed is true this hook must
	// not return Retry:true — the dispatcher enforces this hard rule.
	OnPreStreamResponse(fc *ForwardContext, resp *http.Response) (RetryDecision, error)

	// OnPostResponse is called after the response has been fully forwarded
	// to the client (stream complete or non-SSE body copied). Used for
	// observability and per-account TTL recording. Must not block.
	OnPostResponse(fc *ForwardContext, result ForwardResult)

	// TransformStaticPayload is called during prompt assembly, before the
	// request body is serialised. Implementations may normalise large stable
	// blocks to improve cache hit rates. Errors are swallowed (fail-open);
	// the original payload is used unchanged on any error.
	TransformStaticPayload(fc *ForwardContext, payload *AssembledPrompt) error
}

// RequestContext carries per-request metadata derived from the inbound call.
type RequestContext struct {
	ThreadID   string
	SessionID  string
	IsSubagent bool
	ReqIdx     int
	MsgCount   int
}

// ForwardContext is the mutable state carrier threaded through every hook call
// for a single upstream request attempt. It is not safe for concurrent use;
// the retry loop owns it exclusively between hook calls.
type ForwardContext struct {
	// ReqCtx holds per-request metadata that must be preserved across retries.
	ReqCtx RequestContext

	// OriginalBody is the serialised request body captured before the first
	// Do() call. The retry loop rebuilds OutboundReq.Body from this slice on
	// each attempt so the body is never consumed twice.
	OriginalBody []byte

	// OutboundReq is the *http.Request being sent to the upstream API.
	// BeforeForward may mutate headers (e.g. inject Authorization).
	// Never add SMM-internal bookkeeping headers to this request — they
	// would be forwarded to Anthropic.
	OutboundReq *http.Request

	// Attempt is the zero-based retry counter. 0 = first attempt.
	Attempt int

	// SelectedAccount is the AccountRef chosen by BeforeForward.
	// It is stored here (not in a header) so OnPreStreamResponse can read
	// the full ref without reconstructing it from a string.
	// The field type is interface{} to avoid an import cycle between
	// proxyext and proxyext/accountpool; extension.go performs the
	// type assertion internally.
	SelectedAccount interface{}

	// BytesFlushed is set to true by proxy_forward.go immediately before
	// w.WriteHeader() is called. Once true, no retry is permitted regardless
	// of the upstream status code. The dispatcher in hooks.go reads this
	// field and overrides any Retry:true decision with Retry:false.
	BytesFlushed bool
}

// AssembledPrompt represents the prompt payload during the assembly stage,
// before it is serialised into the request body.
type AssembledPrompt struct {
	// Blocks holds the ordered message/content blocks. Order must be
	// preserved byte-for-byte by any transform to avoid breaking
	// prompt_cache.go block-ordering invariants.
	Blocks []interface{}

	// Raw is the serialised form, populated after assembly. If a transform
	// modifies Blocks it must also re-serialise Raw.
	Raw []byte
}

// RetryDecision is returned by OnPreStreamResponse to instruct the retry loop.
type RetryDecision struct {
	// Retry signals that the request should be retried on a different account.
	// The dispatcher overrides this to false when ForwardContext.BytesFlushed
	// is true — that check is categorical and cannot be bypassed.
	Retry bool

	// Reason is a short human-readable string logged at INFO level.
	// Must not contain token values or auth material.
	Reason string
}

// ForwardResult carries post-response metadata passed to OnPostResponse.
type ForwardResult struct {
	// StatusCode is the HTTP status returned by the upstream API.
	StatusCode int

	// BytesForwarded is the number of bytes written to the client.
	BytesForwarded int64

	// CacheReadTokens and CacheCreationTokens are extracted from the
	// upstream response headers for per-account TTL observation.
	CacheReadTokens     int
	CacheCreationTokens int

	// Err holds any terminal error (network failure, context cancellation).
	Err error
}

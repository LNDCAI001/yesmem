package proxyext

import "net/http"

// RequestContext carries per-request metadata that the SMM extension needs
// to make routing and gating decisions. It is populated by the proxy before
// calling BeforeForward.
type RequestContext struct {
	// ThreadID is the yesmem thread/session identifier. It is preserved
	// across account rotation — rotating accounts never changes this value.
	ThreadID string

	// IsSubagent is true when the request originates from a subagent.
	// Used to gate apply_to_subagents behaviour.
	IsSubagent bool
}

// ForwardContext is the mutable state passed through the hook lifecycle for
// a single request attempt. A new ForwardContext is created for each top-level
// call to SMMForwardWithRetry; Attempt is incremented per retry.
type ForwardContext struct {
	// ReqCtx is the immutable per-request metadata. Never mutated after
	// construction.
	ReqCtx RequestContext

	// OutboundReq is the *http.Request that will be sent to Anthropic.
	// BeforeForward may mutate its headers (auth injection only).
	// It must never be nil when BeforeForward is called.
	OutboundReq *http.Request

	// OriginalBody is the request body bytes, preserved across retries.
	// The proxy drains origReq.Body exactly once before the retry loop;
	// SMMForwardWithRetry wraps these bytes in a fresh bytes.Reader per attempt.
	OriginalBody []byte

	// Attempt is zero-indexed. 0 = first attempt, 1 = first retry, etc.
	// Incremented by the retry loop before each call to BeforeForward.
	Attempt int

	// BytesFlushed is set to true the moment any response bytes are written
	// to the client. It must be set before OnPreStreamResponse is called on
	// any retry attempt. The hooks.go dispatcher enforces Retry:false when
	// this is true — this field is defence-in-depth at the implementation level.
	BytesFlushed bool

	// SelectedAccount holds the AccountRef chosen by BeforeForward for this
	// attempt. It is stored here — not in an outbound header — so that:
	//   1. Anthropic never receives account identity information.
	//   2. OnPreStreamResponse can retrieve the full ref without
	//      reconstructing from a name string (which loses CredentialDir
	//      and Priority).
	//
	// Type is interface{} to keep types.go free of the accountpool import
	// (types.go defines the shared contract between proxyext and internal/proxy;
	// it must not pull in sub-package dependencies). The concrete type is
	// accountpool.AccountRef; callers that need the full ref use:
	//   acc, ok := fc.SelectedAccount.(accountpool.AccountRef)
	//
	// A nil value means no account was selected (pool disabled or fail-open).
	SelectedAccount interface{}
}

// RetryDecision is returned by OnPreStreamResponse to indicate whether the
// retry loop should advance to the next account.
type RetryDecision struct {
	// Retry is true if the caller should close the current response body and
	// attempt the request again with the next available account.
	// Retry must never be true if BytesFlushed was true when the hook was called.
	Retry bool

	// Reason is a short machine-readable label for structured logging.
	// Examples: "quota_limited", "token_invalid", "bytes_already_flushed",
	// "max_retries_exceeded", "not_retryable".
	Reason string
}

// ForwardResult carries the outcome of a completed request for OnPostResponse.
type ForwardResult struct {
	// StatusCode is the HTTP status code of the response actually sent to
	// the client.
	StatusCode int

	// RespHeader carries the upstream response headers for a 2xx success.
	// It is used to capture anthropic-ratelimit-* headers for per-account
	// usage tracking. Nil for transport errors / non-2xx outcomes.
	RespHeader http.Header

	// CacheReadTokens and CacheCreationTokens are extracted from the
	// upstream response usage block for per-account cache TTL observation.
	CacheReadTokens     int
	CacheCreationTokens int

	// Err is non-nil if the request ended in a transport error or a hook
	// error. HTTP-level errors (4xx/5xx) are expressed via StatusCode, not Err.
	Err error
}

// AssembledPrompt is the mutable prompt structure passed to
// TransformStaticPayload. staticplan may rewrite eligible blocks in-place.
// The proxy serialises this struct to JSON after TransformStaticPayload returns.
type AssembledPrompt struct {
	// System is the system prompt as raw JSON bytes. Using json.RawMessage
	// preserves byte-exact content and makes the intent explicit — this is
	// a JSON blob, not an arbitrary Go value. staticplan will unmarshal
	// only when it needs to inspect the content.
	System []byte // json.RawMessage

	// Tools is the tools array as raw JSON bytes. Large stable tool
	// descriptions are the primary staticplan target.
	Tools []byte // json.RawMessage

	// Raw is the full request body as parsed JSON, for transforms that need
	// to inspect fields that System and Tools do not expose.
	Raw map[string]interface{}
}

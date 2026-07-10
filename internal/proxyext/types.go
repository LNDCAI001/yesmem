package proxyext

import "net/http"

// RequestContext carries per-request metadata available to all hooks.
// Fields are read-only from hook implementations — mutations are ignored.
type RequestContext struct {
	// ThreadID is the yesmem thread/session identifier.
	// INVARIANT: preserved across account rotation. Never changes mid-retry.
	ThreadID string

	// IsSubagent is true when the request originates from a subagent.
	// Checked by BeforeForward to enforce apply_to_subagents policy.
	IsSubagent bool

	// MessageCount is the number of messages in the current conversation.
	MessageCount int
}

// ForwardContext is the mutable state carried across hook calls for one
// request attempt. A new ForwardContext is created per retry loop iteration
// by SMMForwardWithRetry in proxy_forward_smm.go.
type ForwardContext struct {
	// ReqCtx holds per-request read-only metadata.
	ReqCtx RequestContext

	// OriginalBody is the drained request body bytes.
	// INVARIANT: never nil after SMMForwardWithRetry sets it.
	// INVARIANT: never modified. Passed to bytes.NewReader on every retry.
	OriginalBody []byte

	// OutboundReq is the *http.Request that will be sent to the upstream.
	// BeforeForward may modify its headers (specifically Authorization).
	// INVARIANT: X-SMM-Account is NEVER set on this request. Account
	// identity is stored in SelectedAccount, not in HTTP headers.
	OutboundReq *http.Request

	// Attempt is the zero-based retry attempt index.
	// 0 = first attempt, 1 = first retry, etc.
	Attempt int

	// BytesFlushed is set to true by SMMForwardWithRetry immediately
	// before w.WriteHeader is called. Any code that observes BytesFlushed==true
	// MUST NOT trigger a retry.
	//
	// INVARIANT: BeforeForward and OnPreStreamResponse are never called
	// when BytesFlushed==true. This invariant is enforced by the
	// hooks.go dispatcher AND by SMMForwardWithRetry.
	BytesFlushed bool

	// SelectedAccount carries the full AccountRef chosen by BeforeForward.
	// Stored as interface{} to avoid an import cycle between proxyext
	// and proxyext/accountpool. The concrete type is accountpool.AccountRef.
	// OnPreStreamResponse type-asserts this value internally.
	//
	// WHY interface{}: proxyext imports proxyext/accountpool. If types.go
	// imported accountpool directly, any addition to accountpool's types
	// would require recompiling all of proxyext. The interface{} indirection
	// breaks the coupling at the cost of one type assertion per response.
	// This is the same pattern used by context.Value in the stdlib.
	//
	// INVARIANT: Never nil after a successful BeforeForward call.
	// INVARIANT: Never appears in log output, error strings, or HTTP headers.
	SelectedAccount interface{}
}

// RetryDecision is returned by OnPreStreamResponse to instruct the
// retry loop in SMMForwardWithRetry.
type RetryDecision struct {
	// Retry is true when the loop should close the current response,
	// advance to the next account, and retry the request.
	// INVARIANT: Retry==true is only honoured when fc.BytesFlushed==false.
	// The dispatcher in hooks.go enforces this before calling through.
	Retry bool

	// Reason is a short machine-readable string for structured logging.
	// Examples: "quota_limited", "token_invalid", "max_retries_exceeded".
	// Must not contain sensitive data (tokens, account names).
	Reason string
}

// ForwardResult carries the outcome after the SSE stream completes.
// Passed to OnPostResponse for cache-TTL and usage accounting.
type ForwardResult struct {
	// InputTokens is the total input token count from the response.
	InputTokens int
	// OutputTokens is the total output token count.
	OutputTokens int
	// CacheReadTokens is the cache_read_input_tokens value.
	CacheReadTokens int
	// CacheWriteTokens is the cache_creation_input_tokens value.
	CacheWriteTokens int
	// StatusCode is the HTTP status code of the response.
	StatusCode int
}

// AssembledPrompt is passed to TransformStaticPayload before the request
// is forwarded. It exposes the mutable system and tool-definition blocks
// that staticplan is permitted to normalise.
type AssembledPrompt struct {
	// SystemBlocks is the list of system prompt content blocks.
	// TransformStaticPayload may normalise text content in place.
	// INVARIANT: len(SystemBlocks) must not change after transform.
	SystemBlocks []map[string]interface{}

	// ToolDefinitions is the list of tool definition objects.
	// TransformStaticPayload may normalise description fields in place.
	// INVARIANT: tool names and input_schema must not be modified.
	ToolDefinitions []map[string]interface{}

	// ContentHash is set by staticplan after hashing the stable content.
	// Used for cache lookup on the next call with identical content.
	ContentHash string
}

// Hooks is the interface that all hook implementations must satisfy.
// The noop implementation in noop.go must be zero-allocation for
// the common case where SMM is disabled.
//
// CONCURRENCY: All methods may be called concurrently from multiple
// goroutines. Implementations must be safe for concurrent use.
type Hooks interface {
	// BeforeForward is called after the outbound request is built but
	// before s.httpClient.Do(proxyReq). Implementations may modify
	// fc.OutboundReq.Header to inject auth.
	//
	// INVARIANT: must not set X-SMM-Account or any SMM-specific header
	// on fc.OutboundReq — those would be forwarded to the upstream API.
	// Store account identity in fc.SelectedAccount only.
	BeforeForward(ctx interface{ Value(key any) any }, fc *ForwardContext) error

	// OnPreStreamResponse is called after s.httpClient.Do returns but
	// before w.WriteHeader. This is the only window where a retry
	// decision can be made.
	//
	// INVARIANT: if fc.BytesFlushed==true, must return RetryDecision{Retry:false}.
	// The dispatcher in hooks.go enforces this regardless of what the
	// implementation returns.
	OnPreStreamResponse(ctx interface{ Value(key any) any }, fc *ForwardContext, resp *http.Response) (RetryDecision, error)

	// OnPostResponse is called after the SSE stream completes.
	// Used for cache-TTL observation and usage accounting.
	// Must not block the response path — called in a goroutine by the dispatcher.
	OnPostResponse(ctx interface{ Value(key any) any }, fc *ForwardContext, result ForwardResult)

	// TransformStaticPayload is called before the request is forwarded
	// when staticplan is enabled. May modify SystemBlocks and ToolDefinitions
	// in place for normalisation. Must be deterministic for the same input.
	//
	// INVARIANT: must not drop content. Must not change len(SystemBlocks)
	// or len(ToolDefinitions). Must not modify tool names or input_schema.
	// INVARIANT: errors are swallowed by the dispatcher (fail-open contract).
	TransformStaticPayload(ctx interface{ Value(key any) any }, reqCtx RequestContext, assembled *AssembledPrompt) error
}

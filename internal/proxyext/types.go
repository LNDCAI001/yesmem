package proxyext

import (
	"context"
	"net/http"
)

// RequestContext carries per-request identity metadata through the extension layer.
// Account identity must never enter prompt text — these fields are routing-only.
type RequestContext struct {
	ThreadID    string
	SessionID   string
	Model       string
	IsSubagent  bool
	Provider    string
	SourceAgent string
}

// ForwardContext is the full state available to hooks at forwarding time.
type ForwardContext struct {
	ReqCtx       RequestContext
	OriginalBody []byte
	OutboundReq  *http.Request
	Attempt      int
}

// RetryDecision is returned by OnPreStreamResponse to tell the caller
// whether to retry with a different account, and why.
// Retry is only ever honoured BEFORE w.WriteHeader has been called.
type RetryDecision struct {
	Retry       bool
	RetryReason string
	NextAccount string // opaque hint, may be empty
}

// ForwardResult summarises what happened after a forward attempt.
// StreamStarted and BytesFlushed are the safety gates for retry logic.
type ForwardResult struct {
	StatusCode        int
	StreamStarted     bool
	BytesFlushed      bool
	ClassifiedFailure string // e.g. "quota_limited", "token_invalid", ""
}

// AssembledPrompt is a view of the prompt payload at the point
// TransformStaticPayload is called. Fields may be nil if not present.
type AssembledPrompt struct {
	SystemBlocks []PromptBlock
	Messages     []PromptBlock
	ToolDocs     []PromptBlock
}

// PromptBlock is a single identifiable content segment.
type PromptBlock struct {
	Role        string
	ContentType string // "text", "tool_doc", "wiki", "briefing", etc.
	Content     string
	ByteLen     int
	Stable      bool // hint: caller believes this content is invariant
}

// AccountRef identifies a configured account by name.
type AccountRef struct {
	Name          string
	CredentialDir string
}

// AccountResult is reported back to the selector after each attempt.
type AccountResult struct {
	Account           AccountRef
	ClassifiedFailure string
	StreamStarted     bool
	BytesFlushed      bool
	Success           bool
}

// TokenResult carries an access token and optional expiry hint.
type TokenResult struct {
	AccessToken string
	ExpiresAt   int64 // unix seconds, 0 = unknown
}

// TokenProvider loads or refreshes an OAuth access token for a given account.
// Implementation must not log raw token values.
type TokenProvider interface {
	GetAccessToken(ctx context.Context, account AccountRef) (TokenResult, error)
}

// AccountSelector picks an account for each outbound request and records outcomes.
type AccountSelector interface {
	Select(ctx context.Context, fc *ForwardContext) (AccountRef, error)
	MarkResult(result AccountResult)
}

// Package accountpool implements multi-account Claude OAuth rotation for SMM.
// It is safe to use concurrently. All state mutations are protected by mu.
package accountpool

import (
	"context"
	"net/http"
	"time"
)

// AccountRef identifies a configured account by name.
type AccountRef struct {
	Name          string
	CredentialDir string
	Priority      int
}

// AccountState tracks runtime health per account.
type AccountState struct {
	Ref               AccountRef
	Status            AccountStatus
	CooldownUntil     time.Time
	LastSelectedAt    time.Time
	LastSuccessAt     time.Time
	LastQuotaHitAt    time.Time
	LastAuthErrorAt   time.Time
	ConsecutiveFails  int
}

// AccountStatus is the runtime health classification for an account.
type AccountStatus int

const (
	StatusAvailable AccountStatus = iota
	StatusCooldown
	StatusHardFailed
)

// TokenResult holds a resolved access token and optional metadata.
type TokenResult struct {
	Token     string
	ExpiresAt time.Time // zero value = unknown
}

// RequestMeta is the per-request context fed to the selector.
type RequestMeta struct {
	ThreadID   string
	SessionID  string
	Model      string
	IsSubagent bool
}

// AccountResult is reported back to the selector after a forwarding attempt.
type AccountResult struct {
	Account           AccountRef
	HTTPStatus        int
	StreamStarted     bool
	BytesFlushed      bool
	ClassifiedFailure FailureClass
}

// FailureClass classifies upstream failures for retry routing.
type FailureClass int

const (
	FailureNone FailureClass = iota
	FailureQuotaLimited    // 429: retry with next account
	FailureTokenInvalid    // 401 before stream: retry with next account
	FailureEntitlement     // 403: do not retry
	FailureUpstreamTransient // 5xx before stream: do not rotate
	FailureNetworkTimeout  // connection timeout before first byte: do not rotate
	FailureStreamMidway    // any failure after stream started: never retry
)

// IsRetryable returns true iff the failure class warrants trying the next account.
// StreamMidway, Entitlement, UpstreamTransient, and NetworkTimeout are never
// retried on a different account.
func (f FailureClass) IsRetryable() bool {
	switch f {
	case FailureQuotaLimited, FailureTokenInvalid:
		return true
	default:
		return false
	}
}

// TokenProvider loads an access token for a given account.
type TokenProvider interface {
	GetAccessToken(ctx context.Context, account AccountRef) (TokenResult, error)
}

// Selector chooses an account and records outcomes.
type Selector interface {
	Select(ctx context.Context, meta RequestMeta) (AccountRef, error)
	MarkResult(result AccountResult)
}

// Injector applies the resolved token to an outbound request.
type Injector interface {
	InjectAuth(req *http.Request, token TokenResult)
}

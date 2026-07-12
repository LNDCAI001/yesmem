// Package accountpool implements multi-account Claude OAuth rotation for SMM.
// It is safe to use concurrently. All state mutations are protected by mu.
package accountpool

import (
	"context"
	"fmt"
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
	Ref              AccountRef
	Status           AccountStatus
	CooldownUntil    time.Time
	LastSelectedAt   time.Time
	LastSuccessAt    time.Time
	LastQuotaHitAt   time.Time
	LastAuthErrorAt  time.Time
	ConsecutiveFails int
	RateLimits       RateLimitSnapshot
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
	// RespHeader carries upstream response headers (for 2xx successes) so
	// the store can capture anthropic-ratelimit-* usage snapshots.
	RespHeader http.Header
}

// FailureClass classifies upstream failures for retry routing.
type FailureClass int

const (
	FailureNone              FailureClass = iota
	FailureQuotaLimited                   // 429: retry with next account
	FailureTokenInvalid                   // 401 before stream: retry with next account
	FailureEntitlement                    // 403: do not retry this attempt
	FailureUpstreamTransient              // 5xx before stream: do not rotate
	FailureNetworkTimeout                 // connection timeout before first byte: do not rotate
	FailureStreamMidway                   // any failure after stream started: never retry
)

// IsRetryable returns true iff the failure class warrants trying the next account.
func (f FailureClass) IsRetryable() bool {
	switch f {
	case FailureQuotaLimited, FailureTokenInvalid:
		return true
	default:
		return false
	}
}

// String returns a short machine-readable label suitable for structured logging.
func (f FailureClass) String() string {
	switch f {
	case FailureNone:
		return "none"
	case FailureQuotaLimited:
		return "quota_limited"
	case FailureTokenInvalid:
		return "token_invalid"
	case FailureEntitlement:
		return "entitlement_mismatch"
	case FailureUpstreamTransient:
		return "upstream_transient"
	case FailureNetworkTimeout:
		return "network_timeout"
	case FailureStreamMidway:
		return "stream_midway"
	default:
		return fmt.Sprintf("unknown(%d)", int(f))
	}
}

// TokenProvider loads an access token for a given account.
type TokenProvider interface {
	GetAccessToken(ctx context.Context, account AccountRef) (TokenResult, error)
	// RefreshAccessToken proactively refreshes the OAuth token for the given
	// account by calling the Claude OAuth refresh endpoint. It writes the
	// updated credentials back to disk on success. A no-op when the token
	// is still valid (more than 5 minutes until expiry).
	RefreshAccessToken(ctx context.Context, account AccountRef) error
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

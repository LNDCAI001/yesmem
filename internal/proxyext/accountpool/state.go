// Package accountpool manages Claude subscription OAuth account selection,
// health tracking, and cooldown state for the SMM proxy extension.
//
// OAuth scope: reads ~/.claude/... credential files written by Claude Code itself.
// This is local-process credential reuse, not a third-party OAuth flow.
package accountpool

import (
	"sync"
	"time"
)

// Status represents the current health of an account slot.
type Status string

const (
	StatusAvailable    Status = "available"
	StatusCooldown     Status = "cooldown"
	StatusAuthFailed   Status = "auth_failed"
	StatusExhausted    Status = "exhausted"
)

// AccountState holds runtime health state for one configured account.
// All fields are safe for concurrent access via the embedded mutex.
type AccountState struct {
	mu sync.RWMutex

	Name          string
	CredentialDir string
	Priority      int

	Status           Status
	CooldownUntil    time.Time
	LastSelectedAt   time.Time
	LastSuccessAt    time.Time
	LastQuotaHitAt   time.Time
	LastAuthErrorAt  time.Time
	ConsecFailures   int

	// CacheObservation tracks what cache warmth this account has seen,
	// so that TTL inference is not confused by cross-account rotation.
	CacheObservation string // e.g. "warm", "cold", "unknown"
}

// IsAvailable returns true if the account can be selected right now.
func (s *AccountState) IsAvailable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Status == StatusAuthFailed || s.Status == StatusExhausted {
		return false
	}
	if s.Status == StatusCooldown && time.Now().Before(s.CooldownUntil) {
		return false
	}
	return true
}

// MarkQuotaHit puts the account into cooldown for the given duration.
func (s *AccountState) MarkQuotaHit(cooldown time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = StatusCooldown
	s.CooldownUntil = time.Now().Add(cooldown)
	s.LastQuotaHitAt = time.Now()
	s.ConsecFailures++
}

// MarkSuccess resets failure state and marks the account healthy.
func (s *AccountState) MarkSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = StatusAvailable
	s.LastSuccessAt = time.Now()
	s.ConsecFailures = 0
}

// MarkAuthFailed marks the account as having a permanent-ish auth error.
// It will not be retried until manually reset or process restart.
func (s *AccountState) MarkAuthFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = StatusAuthFailed
	s.LastAuthErrorAt = time.Now()
	s.ConsecFailures++
}

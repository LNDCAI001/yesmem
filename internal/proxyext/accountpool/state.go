package accountpool

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimitSnapshot captures Anthropic per-account rate-limit headers from a
// successful response so operators can see remaining 5h-session and 7d-weekly
// budgets and their reset times.
type RateLimitSnapshot struct {
	InputTokensRemaining  int64
	InputTokensLimit      int64
	InputTokensReset      time.Time
	WeeklyTokensRemaining int64
	WeeklyTokensLimit     int64
	WeeklyTokensReset     time.Time
	CapturedAt            time.Time
}

// captureRateLimits reads Anthropic rate-limit response headers. Empty/unknown
// headers leave the numeric fields at -1 and times at zero so callers can tell
// "not provided" apart from "zero remaining".
func captureRateLimits(h http.Header) RateLimitSnapshot {
	rl := RateLimitSnapshot{InputTokensRemaining: -1, InputTokensLimit: -1, WeeklyTokensRemaining: -1, WeeklyTokensLimit: -1, CapturedAt: time.Now()}
	if v := h.Get("anthropic-ratelimit-input-tokens-remaining"); v != "" {
		if n, err := parseInt64(v); err == nil {
			rl.InputTokensRemaining = n
		}
	}
	if v := h.Get("anthropic-ratelimit-input-tokens-limit"); v != "" {
		if n, err := parseInt64(v); err == nil {
			rl.InputTokensLimit = n
		}
	}
	if v := h.Get("anthropic-ratelimit-input-tokens-reset"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			rl.InputTokensReset = t
		}
	}
	if v := h.Get("anthropic-ratelimit-input-tokens-weekly-remaining"); v != "" {
		if n, err := parseInt64(v); err == nil {
			rl.WeeklyTokensRemaining = n
		}
	}
	if v := h.Get("anthropic-ratelimit-input-tokens-weekly-limit"); v != "" {
		if n, err := parseInt64(v); err == nil {
			rl.WeeklyTokensLimit = n
		}
	}
	if v := h.Get("anthropic-ratelimit-input-tokens-weekly-reset"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			rl.WeeklyTokensReset = t
		}
	}
	return rl
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

// StateStore holds runtime health state for every configured account.
// All methods are safe for concurrent use.
type StateStore struct {
	mu     sync.Mutex
	states map[string]*AccountState // keyed by account Name
}

// NewStateStore creates an initialised store for the given accounts.
func NewStateStore(accounts []AccountRef) *StateStore {
	s := &StateStore{
		states: make(map[string]*AccountState, len(accounts)),
	}
	for _, a := range accounts {
		a := a
		s.states[a.Name] = &AccountState{Ref: a, Status: StatusAvailable}
	}
	return s
}

// Get returns a copy of the current state for account name.
func (s *StateStore) Get(name string) (AccountState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	if !ok {
		return AccountState{}, false
	}
	return *st, true
}

// IsAvailable reports whether the named account is not in cooldown and not hard-failed.
func (s *StateStore) IsAvailable(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	if !ok {
		return false
	}
	if st.Status == StatusHardFailed {
		return false
	}
	if st.Status == StatusCooldown && time.Now().Before(st.CooldownUntil) {
		return false
	}
	// Cooldown expired — restore to available.
	if st.Status == StatusCooldown {
		st.Status = StatusAvailable
	}
	return true
}

// RecordSuccess marks the account healthy and resets consecutive failure count.
// respHeader (may be nil) is inspected for anthropic-ratelimit-* headers so the
// latest usage snapshot is stored for the /accounts status endpoint.
func (s *StateStore) RecordSuccess(name string, respHeader http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	if !ok {
		return
	}
	st.Status = StatusAvailable
	st.LastSuccessAt = time.Now()
	st.ConsecutiveFails = 0
	if respHeader != nil {
		st.RateLimits = captureRateLimits(respHeader)
	}
}

// RecordQuotaHit places the account in cooldown for the given duration.
// ConsecutiveFails is incremented once.
func (s *StateStore) RecordQuotaHit(name string, cooldown time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	if !ok {
		return
	}
	st.Status = StatusCooldown
	st.CooldownUntil = time.Now().Add(cooldown)
	st.LastQuotaHitAt = time.Now()
	st.ConsecutiveFails++
}

// RecordAuthError marks the account as having had an auth error.
// ConsecutiveFails is incremented once. After maxConsecFails consecutive
// auth errors the account is hard-failed.
func (s *StateStore) RecordAuthError(name string, maxConsecFails int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	if !ok {
		return
	}
	st.LastAuthErrorAt = time.Now()
	st.ConsecutiveFails++
	if maxConsecFails > 0 && st.ConsecutiveFails >= maxConsecFails {
		st.Status = StatusHardFailed
	}
}

// RecordEntitlementError places the account in a short cooldown for a 403
// response. Unlike RecordQuotaHit it does NOT increment ConsecutiveFails —
// the caller is responsible for calling RecordAuthError separately only when
// it wants to count toward the hard-fail threshold.
//
// Motivation: the previous code called RecordQuotaHit (which increments
// ConsecutiveFails) and then RecordAuthError (which increments it again),
// meaning a single 403 burned two counts. RecordEntitlementError fixes this
// by setting the cooldown without touching ConsecutiveFails.
func (s *StateStore) RecordEntitlementError(name string, cooldown time.Duration, maxConsecFails int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	if !ok {
		return
	}
	st.Status = StatusCooldown
	st.CooldownUntil = time.Now().Add(cooldown)
	st.LastAuthErrorAt = time.Now()
	st.ConsecutiveFails++
	if maxConsecFails > 0 && st.ConsecutiveFails >= maxConsecFails {
		st.Status = StatusHardFailed
	}
}

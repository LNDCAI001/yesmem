package accountpool

import (
	"fmt"
	"net/http"
	"strings"
	"strconv"
	"sync"
	"time"
)

// RateLimitSnapshot captures Anthropic per-account rate-limit headers from a
// successful response so operators can see remaining 5h-session and 7d-weekly
// budgets and their reset times.
// RateLimitSnapshot captures Anthropic per-account rate-limit headers from a
// successful response. For Claude Max/Pro subscriptions Anthropic emits the
// "Unified" window headers (Anthropic-Ratelimit-Unified-5h-* and -7d-*): a
// utilization fraction (0..1) plus an absolute reset epoch and a status. These
// directly answer "how much of my 5h / 7d budget is left" without needing the
// older input-tokens-* headers (which some tiers do not send).
type RateLimitSnapshot struct {
	Unified5hUtil            float64
	Unified5hReset          int64
	Unified7dUtil            float64
	Unified7dReset          int64
	UnifiedStatus           string
	UnifiedRepresentativeClaim string
	CapturedAt              time.Time
}

// captureRateLimits reads Anthropic rate-limit response headers. Empty/unknown
// headers leave the numeric fields at -1 and times at zero so callers can tell
// "not provided" apart from "zero remaining".
func captureRateLimits(h http.Header) RateLimitSnapshot {
	rl := RateLimitSnapshot{Unified5hUtil: -1, Unified7dUtil: -1, CapturedAt: time.Now()}
	if v := h.Get("Anthropic-Ratelimit-Unified-5h-Utilization"); v != "" {
		rl.Unified5hUtil = parseFloat64(v)
	}
	if v := h.Get("Anthropic-Ratelimit-Unified-5h-Reset"); v != "" {
		rl.Unified5hReset = parseInt64Default(v, 0)
	}
	if v := h.Get("Anthropic-Ratelimit-Unified-7d-Utilization"); v != "" {
		rl.Unified7dUtil = parseFloat64(v)
	}
	if v := h.Get("Anthropic-Ratelimit-Unified-7d-Reset"); v != "" {
		rl.Unified7dReset = parseInt64Default(v, 0)
	}
	rl.UnifiedStatus = h.Get("Anthropic-Ratelimit-Unified-Status")
	rl.UnifiedRepresentativeClaim = h.Get("Anthropic-Ratelimit-Unified-Representative-Claim")
	return rl
}

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

// parseInt64Default parses an int64 header or returns def on empty/error.
func parseInt64Default(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := parseInt64(s)
	if err != nil {
		return def
	}
	return n
}

// parseFloat64 parses a utilization fraction header (e.g. "0.42") or -1.
func parseFloat64(s string) float64 {
	if s == "" {
		return -1
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1
	}
	return v
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

package accountpool

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Manager orchestrates account selection, auth injection, and pre-stream retry.
type Manager struct {
	config   Config
	selector Selector
	provider TokenProvider
}

// Config holds runtime configuration for the account pool.
type Config struct {
	Enabled             bool
	MaxPreStreamRetries int
	CooldownSeconds     int
	Accounts            []AccountRef
}

// NewManager constructs a Manager from config.
// Returns nil when the pool is disabled or has no configured accounts;
// callers must treat a nil *Manager as a no-op (all methods guard on nil receiver).
func NewManager(cfg Config) *Manager {
	if !cfg.Enabled || len(cfg.Accounts) == 0 {
		return nil
	}
	cooldown := secondsToDuration(cfg.CooldownSeconds, 300)
	return &Manager{
		config:   cfg,
		selector: NewRoundRobinSelector(cfg.Accounts, cooldown),
		provider: &LocalOAuthStore{},
	}
}

// InjectAuth selects an account and injects its Bearer token into req.
// Returns the selected AccountRef so the caller can pass it to ShouldRetry later.
// Returns an error (and AccountRef{}) if no account is available or token retrieval fails.
func (m *Manager) InjectAuth(ctx context.Context, req *http.Request, meta RequestMeta) (AccountRef, error) {
	if m == nil {
		return AccountRef{}, nil
	}
	acc, err := m.selector.Select(ctx, meta)
	if err != nil {
		return AccountRef{}, fmt.Errorf("accountpool: select: %w", err)
	}
	tok, err := m.provider.GetAccessToken(ctx, acc)
	if err != nil {
		// Auth failure on token retrieval: mark invalid and bubble up.
		// The retry loop in proxy_forward_smm.go will call InjectAuth again
		// for the next attempt, which will select a different account.
		m.selector.MarkResult(AccountResult{
			Account:           acc,
			ClassifiedFailure: FailureTokenInvalid,
		})
		return AccountRef{}, fmt.Errorf("accountpool: get token for %q: %w", acc.Name, err)
	}
	// Replace whatever auth the original request carried (API key from IDE
	// plugin, previous rotation attempt, etc.) with this account's Bearer token.
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Del("x-api-key") // Claude Code sometimes sends both; remove stale key.
	log.Printf("[accountpool] injected account=%q", acc.Name)
	return acc, nil
}

// ShouldRetry inspects a pre-stream HTTP response and returns true when the
// request should be retried with a different account.
//
// Invariants enforced here (not by caller):
//   - Always returns false if streamStarted is true.
//   - Always returns false if attempt >= MaxPreStreamRetries.
//   - Calls MarkResult on acc before returning, so caller state is always recorded.
func (m *Manager) ShouldRetry(acc AccountRef, resp *http.Response, streamStarted bool, attempt int) bool {
	if m == nil {
		return false
	}
	if streamStarted {
		// Bytes have been flushed to the client. Retrying would require
		// replaying the stream from scratch, which we cannot do safely.
		return false
	}
	if attempt >= m.config.MaxPreStreamRetries {
		// Caller has exhausted the configured retry budget.
		m.selector.MarkResult(AccountResult{
			Account:           acc,
			ClassifiedFailure: Classify(resp, false),
		})
		return false
	}
	fc := Classify(resp, false)
	m.selector.MarkResult(AccountResult{
		Account:           acc,
		ClassifiedFailure: fc,
	})
	return fc.IsRetryable()
}

// RecordSuccess records a successful (200 + stream delivered) outcome for acc.
func (m *Manager) RecordSuccess(acc AccountRef) {
	if m == nil {
		return
	}
	m.selector.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureNone})
}

// secondsToDuration converts an integer seconds value to time.Duration,
// substituting defaultSecs when secs is zero or negative.
func secondsToDuration(secs, defaultSecs int) time.Duration {
	if secs <= 0 {
		secs = defaultSecs
	}
	return time.Duration(secs) * time.Second
}

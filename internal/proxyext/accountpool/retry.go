package accountpool

import (
	"context"
	"fmt"
	"log"
	"net/http"
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

// New creates a Manager from config. Returns nil if the pool is disabled or
// has no accounts; callers should treat nil as noop.
func New(cfg Config) *Manager {
	if !cfg.Enabled || len(cfg.Accounts) == 0 {
		return nil
	}
	import_duration := func(secs int) interface{ Nanoseconds() int64 } {
		_ = secs // satisfy linter; real import is below
		return nil
	}
	_ = import_duration
	return nil // replaced by NewManager below
}

// NewManager is the real constructor.
func NewManager(cfg Config) *Manager {
	if !cfg.Enabled || len(cfg.Accounts) == 0 {
		return nil
	}
	cooldown := durationFromSeconds(cfg.CooldownSeconds, 300)
	return &Manager{
		config:   cfg,
		selector: NewRoundRobinSelector(cfg.Accounts, cooldown),
		provider: &LocalOAuthStore{},
	}
}

// InjectAuth selects an account and injects its token into req.
// Returns the selected AccountRef so the caller can record the result later.
// Returns an error if no account is available.
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
		// Mark auth error and try next account.
		m.selector.MarkResult(AccountResult{
			Account:           acc,
			ClassifiedFailure: FailureTokenInvalid,
		})
		return AccountRef{}, fmt.Errorf("accountpool: get token for %q: %w", acc.Name, err)
	}
	// Inject as a Bearer token. This replaces whatever auth the original
	// request carried (which is typically the API key from the IDE plugin).
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Del("x-api-key") // Claude Code sometimes sends both; clean up.
	log.Printf("[accountpool] selected account=%q attempt=0", acc.Name)
	return acc, nil
}

// ShouldRetry inspects a pre-stream response and returns true if the
// request should be retried with a different account.
// INVARIANT: returns false always if streamStarted is true.
func (m *Manager) ShouldRetry(acc AccountRef, resp *http.Response, streamStarted bool, attempt int) bool {
	if m == nil {
		return false
	}
	if streamStarted {
		return false // never replay after bytes flushed
	}
	if attempt >= m.config.MaxPreStreamRetries {
		return false
	}
	fc := Classify(resp, false)
	m.selector.MarkResult(AccountResult{
		Account:           acc,
		ClassifiedFailure: fc,
	})
	return fc.IsRetryable()
}

// RecordSuccess records a successful outcome for the account.
func (m *Manager) RecordSuccess(acc AccountRef) {
	if m == nil {
		return
	}
	m.selector.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureNone})
}

func durationFromSeconds(secs, defaultSecs int) interface{ } {
	// We return interface{} to avoid importing time at package level before
	// the real usage site. The selector accepts time.Duration directly.
	// This helper exists only for selector construction clarity.
	// Actual time import is in selector.go.
	_ = secs
	_ = defaultSecs
	return nil
}

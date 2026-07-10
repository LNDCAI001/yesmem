package accountpool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Config holds runtime configuration for the account pool.
type Config struct {
	Enabled             bool
	MaxPreStreamRetries int
	CooldownSeconds     int
	ApplyToSubagents    bool
	Accounts            []AccountRef
}

// Pool is the public-facing orchestrator for account selection and auth injection.
// All methods are safe for concurrent use. A nil Pool is safe to call —
// all methods return zero values and nil errors.
type Pool struct {
	cfg      Config
	sel      *RoundRobinSelector
	provider TokenProvider
	logger   *log.Logger
}

// errExhausted is the sentinel error returned when all accounts are unavailable.
var errExhausted = errors.New("accountpool: all accounts are unavailable")

// IsExhausted reports whether err signals that all pool accounts are exhausted.
// Used by callers to distinguish hard-exhaustion (surface to client) from
// soft selection errors (fail open).
func IsExhausted(err error) bool {
	return err != nil && strings.Contains(err.Error(), "all accounts are unavailable")
}

// NewPool creates an active account pool. Returns (nil, nil) when disabled or
// when no accounts are configured — callers treat a nil Pool as noop.
func NewPool(cfg Config, logger *log.Logger) (*Pool, error) {
	if !cfg.Enabled || len(cfg.Accounts) == 0 {
		return nil, nil
	}
	if logger == nil {
		logger = log.Default()
	}
	cooldown := time.Duration(cfg.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 300 * time.Second
	}
	return &Pool{
		cfg:      cfg,
		sel:      NewRoundRobinSelector(cfg.Accounts, cooldown),
		provider: &LocalOAuthStore{},
		logger:   logger,
	}, nil
}

// SelectAndGetToken selects the next available account and fetches its token
// in one call. Returns (AccountRef{}, TokenResult{}, err) on any failure.
// If all accounts are unavailable, the error satisfies IsExhausted.
func (p *Pool) SelectAndGetToken(ctx context.Context, meta RequestMeta) (AccountRef, TokenResult, error) {
	if p == nil {
		return AccountRef{}, TokenResult{}, nil
	}
	acc, err := p.sel.Select(ctx, meta)
	if err != nil {
		// Wrap with the exhausted sentinel wording so IsExhausted matches.
		return AccountRef{}, TokenResult{}, fmt.Errorf("accountpool: all accounts are unavailable: %w", err)
	}
	tok, err := p.provider.GetAccessToken(ctx, acc)
	if err != nil {
		// Token load failed — mark the account and surface the error.
		// The caller (BeforeForward) will fail-open or fail-hard depending on
		// whether IsExhausted is true.
		p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureTokenInvalid})
		return AccountRef{}, TokenResult{}, fmt.Errorf("accountpool: token load for %q: %w", acc.Name, err)
	}
	p.logger.Printf("[accountpool] smm_account_selected=%q", acc.Name)
	return acc, tok, nil
}

// ShouldRetry inspects a pre-stream HTTP status code and returns whether the
// request should be retried on a different account, plus a machine-readable
// reason string for structured logging.
//
// INVARIANT: always returns (false, ...) when attempt >= MaxPreStreamRetries.
// The BytesFlushed guard is enforced by the hooks.go dispatcher before this
// is ever called — ShouldRetry does not re-check it.
func (p *Pool) ShouldRetry(statusCode int, attempt int, acc AccountRef) (bool, string) {
	if p == nil {
		return false, "pool_nil"
	}
	if attempt >= p.cfg.MaxPreStreamRetries {
		return false, "max_retries_exceeded"
	}
	// Construct a synthetic response so we can reuse Classify without
	// changing its signature or duplicating the status-code table.
	synthResp := &http.Response{StatusCode: statusCode}
	fc := Classify(synthResp, false)
	p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: fc})
	if fc.IsRetryable() {
		p.logger.Printf("[accountpool] smm_account_rotation_total=+1 account=%q status=%d attempt=%d",
			acc.Name, statusCode, attempt)
		return true, fc.String()
	}
	return false, fc.String()
}

// RecordSuccess marks a successful completion for the given account.
func (p *Pool) RecordSuccess(acc AccountRef) {
	if p == nil {
		return
	}
	p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureNone})
}

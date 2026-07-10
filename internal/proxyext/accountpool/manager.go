package accountpool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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

// errExhausted is the typed sentinel returned when all accounts are unavailable.
// IsExhausted uses errors.Is against this value — do not use string matching.
var errExhausted = errors.New("accountpool: all accounts are unavailable")

// IsExhausted reports whether err signals that all pool accounts are exhausted.
// Uses errors.Is on the typed sentinel to avoid false positives from unrelated
// error messages that happen to contain the same substring.
func IsExhausted(err error) bool {
	return errors.Is(err, errExhausted)
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
	// CooldownSeconds=0 means no cooldown — rotate immediately.
	// A positive value sets the cooldown duration after a 429.
	cooldown := time.Duration(cfg.CooldownSeconds) * time.Second
	return &Pool{
		cfg:      cfg,
		sel:      NewRoundRobinSelector(cfg.Accounts, cooldown),
		provider: &LocalOAuthStore{},
		logger:   logger,
	}, nil
}

// SelectAndGetToken selects the next available account and fetches its token.
// Returns (AccountRef{}, TokenResult{}, errExhausted) when all accounts are
// unavailable. IsExhausted(err) reports true for this case.
func (p *Pool) SelectAndGetToken(ctx context.Context, meta RequestMeta) (AccountRef, TokenResult, error) {
	if p == nil {
		return AccountRef{}, TokenResult{}, nil
	}
	acc, err := p.sel.Select(ctx, meta)
	if err != nil {
		// Wrap the typed sentinel so callers can use errors.Is.
		return AccountRef{}, TokenResult{}, fmt.Errorf("%w: %w", errExhausted, err)
	}
	tok, err := p.provider.GetAccessToken(ctx, acc)
	if err != nil {
		// Token load failed — mark the account so Select skips it next round.
		p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureTokenInvalid})
		return AccountRef{}, TokenResult{}, fmt.Errorf("accountpool: token load for %q: %w", acc.Name, err)
	}
	p.logger.Printf("[accountpool] smm_account_selected=%q", acc.Name)
	return acc, tok, nil
}

// ShouldRetry inspects a pre-stream HTTP status code and returns whether the
// request should be retried on a different account.
//
// INVARIANT: always returns (false, ...) when attempt >= MaxPreStreamRetries.
// The BytesFlushed guard is enforced by hooks.go before this is called.
//
// Note: synthResp has only StatusCode set. Classify reads only StatusCode
// (and the streamStarted bool which is always false here), so the nil Body
// and nil Header are intentional and safe.
func (p *Pool) ShouldRetry(statusCode int, attempt int, acc AccountRef) (bool, string) {
	if p == nil {
		return false, "pool_nil"
	}
	if attempt >= p.cfg.MaxPreStreamRetries {
		return false, "max_retries_exceeded"
	}
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

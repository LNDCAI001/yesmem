package accountpool

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Pool is the public-facing orchestrator. It wraps Manager and exposes
// the methods called from proxy_forward.go via proxyext hooks.
type Pool struct {
	cfg      Config
	sel      *RoundRobinSelector
	provider TokenProvider
}

// NewPool creates an active account pool. Returns nil when disabled.
func NewPool(cfg Config) *Pool {
	if !cfg.Enabled || len(cfg.Accounts) == 0 {
		return nil
	}
	cooldown := time.Duration(cfg.CooldownSeconds) * time.Second
	if cooldown <= 0 {
		cooldown = 300 * time.Second
	}
	return &Pool{
		cfg:      cfg,
		sel:      NewRoundRobinSelector(cfg.Accounts, cooldown),
		provider: &LocalOAuthStore{},
	}
}

// InjectAuth selects an account and sets the Authorization header on req.
// Returns the selected account ref for later result recording.
func (p *Pool) InjectAuth(ctx context.Context, req *http.Request, meta RequestMeta) (AccountRef, error) {
	if p == nil {
		return AccountRef{}, nil
	}
	acc, err := p.sel.Select(ctx, meta)
	if err != nil {
		return AccountRef{}, fmt.Errorf("accountpool: no available account: %w", err)
	}
	tok, err := p.provider.GetAccessToken(ctx, acc)
	if err != nil {
		p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureTokenInvalid})
		return AccountRef{}, fmt.Errorf("accountpool: token load failed for %q: %w", acc.Name, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Del("x-api-key")
	log.Printf("[accountpool] account=%q attempt=%d", acc.Name, 0)
	return acc, nil
}

// InjectAuthForAttempt is like InjectAuth but logs the attempt number.
func (p *Pool) InjectAuthForAttempt(ctx context.Context, req *http.Request, meta RequestMeta, attempt int) (AccountRef, error) {
	if p == nil {
		return AccountRef{}, nil
	}
	acc, err := p.sel.Select(ctx, meta)
	if err != nil {
		return AccountRef{}, fmt.Errorf("accountpool: no available account: %w", err)
	}
	tok, err := p.provider.GetAccessToken(ctx, acc)
	if err != nil {
		p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureTokenInvalid})
		return AccountRef{}, fmt.Errorf("accountpool: token load failed for %q: %w", acc.Name, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Del("x-api-key")
	log.Printf("[accountpool] account=%q attempt=%d", acc.Name, attempt)
	return acc, nil
}

// ShouldRetry returns true if the response warrants a retry on a different account.
// INVARIANT: always false when streamStarted or attempt >= max.
func (p *Pool) ShouldRetry(acc AccountRef, resp *http.Response, streamStarted bool, attempt int) bool {
	if p == nil || streamStarted {
		return false
	}
	if attempt >= p.cfg.MaxPreStreamRetries {
		return false
	}
	fc := Classify(resp, false)
	p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: fc})
	if fc.IsRetryable() {
		log.Printf("[accountpool] retry account=%q status=%d class=%d attempt=%d",
			acc.Name, resp.StatusCode, fc, attempt)
		return true
	}
	return false
}

// RecordSuccess records a successful completion for the account.
func (p *Pool) RecordSuccess(acc AccountRef) {
	if p == nil {
		return
	}
	p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureNone})
}

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

// snapshot returns a copy of the runtime state for the named account.
// Used by the selection logger to emit a rolling usage line. The second
// return value is false if the account is unknown.
func (p *Pool) snapshot(name string) (AccountState, bool) {
	if p == nil {
		return AccountState{}, false
	}
	return p.sel.snapshot(name)
}

// accountRateLimitView renders the rate-limit snapshot for JSON. If no usage
// data has been captured yet (CapturedAt zero), all count fields are nil so
// the endpoint shows null ("not provided") rather than a misleading 0.
func accountRateLimitView(rl RateLimitSnapshot) RateLimitView {
	if rl.CapturedAt.IsZero() {
		return RateLimitView{CapturedAt: ""}
	}
	return RateLimitView{
		Unified5hUtil:       f64OrNil(rl.Unified5hUtil * 100),
		Unified5hReset:      epochToRFC3339(rl.Unified5hReset),
		Unified7dUtil:       f64OrNil(rl.Unified7dUtil * 100),
		Unified7dReset:      epochToRFC3339(rl.Unified7dReset),
		UnifiedStatus:       rl.UnifiedStatus,
		RepresentativeClaim: rl.UnifiedRepresentativeClaim,
		CapturedAt:         isoOrNever(rl.CapturedAt),
	}
}

// f64OrNil returns nil for the -1 "not provided" sentinel, else a pointer to v.
func f64OrNil(v float64) *float64 {
	if v < 0 {
		return nil
	}
	vv := v
	return &vv
}

// epochToRFC3339 renders a unix epoch (seconds) as RFC3339, or "" if zero.
func epochToRFC3339(epoch int64) string {
	if epoch <= 0 {
		return ""
	}
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}

// i64OrNil returns nil for the -1 "not provided" sentinel, else a pointer to v.
func i64OrNil(v int64) *int64 {
	if v < 0 {
		return nil
	}
	vv := v
	return &vv
}

// isoOrNever renders an RFC3339 timestamp, or "never" for the zero time.
func isoOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// NewPool creates an active account pool. Returns (nil, nil) when disabled or
// when no accounts are configured — callers treat a nil Pool as noop.
func NewPool(cfg Config, logger *log.Logger) (*Pool, error) {
	if !cfg.Enabled || len(cfg.Accounts) == 0 {
		return nil, nil
	}
	// Guard: duplicate account names collapse into one StateStore key (the
	// pool is keyed by Name), so two empty/identical names render the second
	// account invisible and a single 429 disables the whole pool. Fail loud
	// instead of silently degrading to one account.
	seen := make(map[string]int)
	for _, a := range cfg.Accounts {
		seen[a.Name]++
	}
	var dups []string
	for n, c := range seen {
		if c > 1 {
			dups = append(dups, n)
		}
	}
	if len(dups) > 0 {
		return nil, fmt.Errorf("accountpool: duplicate account name(s) %v — each account needs a unique name or the pool collapses to one key and a single 429 disables everything", dups)
	}
	if logger == nil {
		logger = log.Default()
	}
	// CooldownSeconds=0 means no cooldown — rotate immediately.
	// A positive value sets the cooldown duration after a 429.
	cooldown := time.Duration(cfg.CooldownSeconds) * time.Second
	pool := &Pool{
		cfg:      cfg,
		sel:      NewRoundRobinSelector(cfg.Accounts, cooldown),
		provider: &LocalOAuthStore{},
		logger:   logger,
	}

	// Warm cache for all configured accounts on startup so the first
	// request gets a fast cache hit. RefreshAccessToken reads the credential
	// file from disk and populates the in-memory cache. It is a no-op on
	// error (disk not readable, missing tokens, etc.).
	for _, acc := range cfg.Accounts {
		_ = pool.provider.RefreshAccessToken(context.Background(), acc)
	}
	logger.Printf("[accountpool] cache warmed for %d account(s)", len(cfg.Accounts))

	return pool, nil
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

	// Emit a rolling usage line if we have a rate-limit snapshot for the
	// selected account, so operators can see remaining 5h/7d budget at a glance.
	if st, ok := p.sel.snapshot(acc.Name); ok && st.RateLimits.CapturedAt != (time.Time{}) {
		rl := st.RateLimits
		p.logger.Printf("[accountpool] smm_usage name=%q 5h_util=%.1f%% 7d_util=%.1f%% status=%s claim=%s",
			acc.Name, rl.Unified5hUtil*100, rl.Unified7dUtil*100, rl.UnifiedStatus, rl.UnifiedRepresentativeClaim)
	}
	return acc, tok, nil
}

// AccountView is a serialisable snapshot of one account's pool state,
// used by the proxy /accounts status endpoint.
type AccountView struct {
	Name              string `json:"name"`
	CredentialDir     string `json:"credential_dir"`
	Status            string `json:"status"`
	ConsecutiveFails  int    `json:"consecutive_fails"`
	LastSelectedAt    string `json:"last_selected_at,omitempty"`
	LastSuccessAt     string `json:"last_success_at,omitempty"`
	CooldownUntil     string `json:"cooldown_until,omitempty"`
	RateLimits        RateLimitView `json:"rate_limits"`
}

// RateLimitView is the JSON form of RateLimitSnapshot.
// RateLimitView is the JSON form of RateLimitSnapshot. Counts are *int64 so an
// upstream that does not emit anthropic-ratelimit-* headers serialises them as
// null ("not provided") rather than a misleading 0/-1.
// RateLimitView is the JSON form of RateLimitSnapshot. Utilization is shown as a
// percent (0..100) and resets as RFC3339; nil/empty means "not provided".
type RateLimitView struct {
	Unified5hUtil      *float64 `json:"unified_5h_utilization_pct,omitempty"`
	Unified5hReset     string   `json:"unified_5h_reset,omitempty"`
	Unified7dUtil      *float64 `json:"unified_7d_utilization_pct,omitempty"`
	Unified7dReset     string   `json:"unified_7d_reset,omitempty"`
	UnifiedStatus      string   `json:"unified_status,omitempty"`
	RepresentativeClaim string  `json:"representative_claim,omitempty"`
	CapturedAt         string   `json:"captured_at,omitempty"`
}

// Snapshot returns a copy of the runtime state for the named account.
func (p *Pool) Snapshot(name string) (AccountState, bool) {
	if p == nil {
		return AccountState{}, false
	}
	return p.sel.snapshot(name)
}

// Accounts returns a view of every account in the pool. A nil pool returns nil.
func (p *Pool) Accounts() []AccountView {
	if p == nil {
		return nil
	}
	views := make([]AccountView, 0, len(p.cfg.Accounts))
	for _, acc := range p.cfg.Accounts {
		st, _ := p.sel.snapshot(acc.Name)
		rl := st.RateLimits
		views = append(views, AccountView{
			Name:          acc.Name,
			CredentialDir: acc.CredentialDir,
			Status:        st.Status.String(),
			ConsecutiveFails: st.ConsecutiveFails,
			LastSelectedAt: isoOrNever(st.LastSelectedAt),
			LastSuccessAt:  isoOrNever(st.LastSuccessAt),
			CooldownUntil:  isoOrNever(st.CooldownUntil),
				RateLimits: accountRateLimitView(rl),

		})
	}
	return views
}

// String renders an AccountStatus for JSON.
func (a AccountStatus) String() string {
	switch a {
	case StatusAvailable:
		return "available"
	case StatusCooldown:
		return "cooldown"
	case StatusHardFailed:
		return "hard_failed"
	default:
		return "unknown"
	}
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
// RecordSuccess marks a successful completion for the given account.
// respHeader (may be nil) carries anthropic-ratelimit-* headers for usage tracking.
func (p *Pool) RecordSuccess(acc AccountRef, respHeader http.Header) {
	if p == nil {
		return
	}
	p.sel.MarkResult(AccountResult{Account: acc, ClassifiedFailure: FailureNone, RespHeader: respHeader})
}

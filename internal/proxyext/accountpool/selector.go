package accountpool

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// Config holds the account pool configuration loaded from YAML.
type Config struct {
	Enabled              bool          `yaml:"enabled"`
	Provider             string        `yaml:"provider"` // must be "claude_oauth" in v1
	Strategy             string        `yaml:"strategy"` // "round_robin" only in v1
	MaxPreStreamRetries  int           `yaml:"max_prestream_retries"`
	CooldownAfter429     time.Duration `yaml:"cooldown_after_429"`
	ApplyToSubagents     bool          `yaml:"apply_to_subagents"`
	Accounts             []AccountConfig `yaml:"accounts"`
}

// AccountConfig is the per-account entry in the YAML config.
type AccountConfig struct {
	Name          string `yaml:"name"`
	CredentialDir string `yaml:"credential_dir"`
	Priority      int    `yaml:"priority"`
}

// RoundRobinSelector implements proxyext.AccountSelector using cooldown-aware
// round robin. All state is in-memory; restarting the process resets cooldowns.
type RoundRobinSelector struct {
	mu       sync.Mutex
	accounts []*AccountState
	cursor   int
	cooldown time.Duration
}

var _ proxyext.AccountSelector = (*RoundRobinSelector)(nil)

// NewRoundRobinSelector builds a selector from config.
func NewRoundRobinSelector(cfg Config) (*RoundRobinSelector, error) {
	if len(cfg.Accounts) == 0 {
		return nil, fmt.Errorf("accountpool: no accounts configured")
	}
	states := make([]*AccountState, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		states = append(states, &AccountState{
			Name:          a.Name,
			CredentialDir: a.CredentialDir,
			Priority:      a.Priority,
			Status:        StatusAvailable,
		})
	}
	cd := cfg.CooldownAfter429
	if cd == 0 {
		cd = 5 * time.Minute
	}
	return &RoundRobinSelector{
		accounts: states,
		cooldown: cd,
	}, nil
}

// Select picks the next available account in round-robin order.
// Returns an error only if all accounts are unavailable.
func (r *RoundRobinSelector) Select(_ context.Context, fc *proxyext.ForwardContext) (proxyext.AccountRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(r.accounts)
	for i := 0; i < n; i++ {
		idx := (r.cursor + i) % n
		acc := r.accounts[idx]
		if acc.IsAvailable() {
			r.cursor = (idx + 1) % n
			acc.mu.Lock()
			acc.LastSelectedAt = time.Now()
			acc.mu.Unlock()
			return proxyext.AccountRef{
				Name:          acc.Name,
				CredentialDir: acc.CredentialDir,
			}, nil
		}
	}
	return proxyext.AccountRef{}, fmt.Errorf("accountpool: all accounts unavailable")
}

// MarkResult updates account health based on the outcome of a forward attempt.
func (r *RoundRobinSelector) MarkResult(result proxyext.AccountResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, acc := range r.accounts {
		if acc.Name != result.Account.Name {
			continue
		}
		switch result.ClassifiedFailure {
		case string(FailureQuotaLimited):
			acc.MarkQuotaHit(r.cooldown)
		case string(FailureTokenInvalid):
			acc.MarkAuthFailed()
		default:
			if result.Success {
				acc.MarkSuccess()
			}
		}
		return
	}
}

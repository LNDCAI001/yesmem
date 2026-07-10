// Package proxyext implements the SMM extension hooks for the yesmem proxy.
// It is intentionally isolated from internal/proxy to minimise upstream merge
// surface. The only files that import this package are:
//
//	internal/proxy/proxy_forward.go  — calls BeforeForward / OnPreStreamResponse
//	internal/proxy/proxy.go          — calls Init at startup
//
package proxyext

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/LNDCAI001/yesmem/internal/proxyext/accountpool"
)

// SMMConfig is the top-level YAML block `smm:` loaded by the yesmem config
// system. All fields default to the zero value (features disabled).
type SMMConfig struct {
	Enabled     bool           `yaml:"enabled"`
	AccountPool AccountPoolCfg `yaml:"account_pool"`
	StaticPlan  StaticPlanCfg  `yaml:"static_plan"`
}

// AccountPoolCfg mirrors the `smm.account_pool` YAML block.
type AccountPoolCfg struct {
	Enabled              bool                    `yaml:"enabled"`
	MaxPreStreamRetries  int                     `yaml:"max_prestream_retries"`
	CooldownSeconds      int                     `yaml:"cooldown_seconds_after_429"`
	ApplyToSubagents     bool                    `yaml:"apply_to_subagents"`
	Accounts             []AccountPoolAccountCfg `yaml:"accounts"`
}

// AccountPoolAccountCfg is one entry in `smm.account_pool.accounts`.
type AccountPoolAccountCfg struct {
	Name          string `yaml:"name"`
	CredentialDir string `yaml:"credential_dir"`
	Priority      int    `yaml:"priority"`
}

// StaticPlanCfg mirrors the `smm.static_plan` YAML block.
// ExperimentalMultimodal is intentionally absent in v1 — it is not in scope
// and encoding it in the config struct would invite accidental enablement.
type StaticPlanCfg struct {
	Enabled          bool   `yaml:"enabled"`
	Mode             string `yaml:"mode"`
	MinBytes         int    `yaml:"min_bytes"`
	CacheByHash      bool   `yaml:"cache_by_hash"`
	FailOpen         bool   `yaml:"fail_open"`
	ApplyToSubagents bool   `yaml:"apply_to_subagents"`
}

// smmHooks is the live implementation returned by NewSMMHooks when SMM is
// enabled. It satisfies the Hooks interface defined in hooks.go.
type smmHooks struct {
	cfg    SMMConfig
	pool   *accountpool.Pool
	logger *log.Logger
}

// NewSMMHooks constructs and returns the active SMM hook implementation.
// If cfg.Enabled is false, DefaultHooks() is returned and the pool is never
// initialised — the noop path has zero overhead beyond a single interface call.
func NewSMMHooks(cfg SMMConfig, logger *log.Logger) (Hooks, error) {
	if !cfg.Enabled {
		return DefaultHooks(), nil
	}

	var pool *accountpool.Pool
	if cfg.AccountPool.Enabled {
		poolCfg := accountpool.Config{
			Enabled:             true,
			MaxPreStreamRetries: cfg.AccountPool.MaxPreStreamRetries,
			CooldownSeconds:     cfg.AccountPool.CooldownSeconds,
			ApplyToSubagents:    cfg.AccountPool.ApplyToSubagents,
		}
		for _, a := range cfg.AccountPool.Accounts {
			poolCfg.Accounts = append(poolCfg.Accounts, accountpool.AccountRef{
				Name:          a.Name,
				CredentialDir: a.CredentialDir,
				Priority:      a.Priority,
			})
		}
		var err error
		pool, err = accountpool.NewPool(poolCfg, logger)
		if err != nil {
			return nil, fmt.Errorf("smm: init account pool: %w", err)
		}
	}

	return &smmHooks{
		cfg:    cfg,
		pool:   pool,
		logger: logger,
	}, nil
}

// BeforeForward selects an account from the pool and injects its Authorization
// header onto fc.OutboundReq. The selected AccountRef is stored in
// fc.SelectedAccount — never in an outbound header — so Anthropic never sees
// SMM-internal bookkeeping.
func (h *smmHooks) BeforeForward(fc *ForwardContext) error {
	if h.pool == nil {
		return nil
	}
	if fc.ReqCtx.IsSubagent && !h.cfg.AccountPool.ApplyToSubagents {
		return nil
	}

	meta := accountpool.RequestMeta{
		ThreadID:   fc.ReqCtx.ThreadID,
		IsSubagent: fc.ReqCtx.IsSubagent,
	}

	acc, tok, err := h.pool.SelectAndGetToken(context.Background(), meta)
	if err != nil {
		// Fail open: if all accounts are exhausted, surface the error so the
		// caller can return a clean failure rather than silently using stale auth.
		// For all other selection errors, log and let the request proceed with
		// the client's original auth (preserves the fail-open constraint).
		if accountpool.IsExhausted(err) {
			return fmt.Errorf("smm: all accounts exhausted: %w", err)
		}
		h.logger.Printf("[smm] smm_account_select_error=true reason=%v (fail open)", err)
		return nil
	}

	// Inject auth. tok.Token is the Bearer string from the OAuth store.
	fc.OutboundReq.Header.Set("Authorization", "Bearer "+tok.Token)
	// Claude Code sometimes sends both Authorization and x-api-key. Remove
	// the API-key header so only the pool-injected token reaches Anthropic.
	fc.OutboundReq.Header.Del("x-api-key")

	// Store the full ref in ForwardContext so OnPreStreamResponse can retrieve
	// it without reconstructing from a name string.
	// Type is interface{} to keep types.go free of the accountpool import.
	fc.SelectedAccount = acc

	h.logger.Printf("[smm] smm_account_selected=%s smm_account_attempt=%d",
		acc.Name, fc.Attempt)
	return nil
}

// OnPreStreamResponse is called after the upstream response headers arrive but
// before any bytes are written to the client. It reads fc.BytesFlushed as the
// categorical gate: if true, no retry is possible and the decision is always
// Retry:false. This check is also enforced by the hooks.go dispatcher, but we
// check it here as defence-in-depth.
func (h *smmHooks) OnPreStreamResponse(fc *ForwardContext, resp *http.Response) (RetryDecision, error) {
	if h.pool == nil {
		return RetryDecision{}, nil
	}

	// Hard rule: never retry after bytes have been flushed to the client.
	// The dispatcher also enforces this, but we check here as defence-in-depth.
	if fc.BytesFlushed {
		return RetryDecision{Retry: false, Reason: "bytes_already_flushed"}, nil
	}

	acc, ok := fc.SelectedAccount.(accountpool.AccountRef)
	if !ok {
		// No account was selected (pool disabled or fail-open fallback).
		return RetryDecision{}, nil
	}

	retry, reason := h.pool.ShouldRetry(resp.StatusCode, fc.Attempt, acc)

	h.logger.Printf("[smm] smm_retry_decision=%v smm_retry_reason=%s smm_account_attempt=%d status=%d",
		retry, reason, fc.Attempt, resp.StatusCode)

	return RetryDecision{Retry: retry, Reason: reason}, nil
}

// OnPostResponse records per-account outcome for state tracking.
func (h *smmHooks) OnPostResponse(fc *ForwardContext, result ForwardResult) {
	if h.pool == nil {
		return
	}
	acc, ok := fc.SelectedAccount.(accountpool.AccountRef)
	if !ok {
		return
	}
	if result.Err == nil && result.StatusCode < 400 {
		h.pool.RecordSuccess(acc)
	}
	// Non-2xx terminal outcomes are already recorded inside ShouldRetry
	// (which marks the account before returning). OnPostResponse only needs
	// to record clean successes that were never passed through ShouldRetry
	// (e.g. non-streaming 200 responses).
}

// TransformStaticPayload is a no-op in v1. staticplan is present in the
// package tree but is not wired into the prompt assembly stage until
// compress_context.go has been audited for overlap.
func (h *smmHooks) TransformStaticPayload(_ *ForwardContext, _ *AssembledPrompt) error {
	return nil
}

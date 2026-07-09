package proxyext

import (
	"context"
	"log"
	"net/http"

	"github.com/carsteneu/yesmem/internal/proxyext/accountpool"
	"github.com/carsteneu/yesmem/internal/proxyext/observability"
	"github.com/carsteneu/yesmem/internal/proxyext/staticplan"
)

// SMMConfig is the top-level configuration for the SMM extension layer.
// It maps directly to the `smm:` block in yesmem's config.yaml.
type SMMConfig struct {
	Enabled     bool                `yaml:"enabled"`
	AccountPool AccountPoolConfig   `yaml:"account_pool"`
	StaticPlan  staticplan.Config   `yaml:"static_plan"`
}

// AccountPoolConfig is the yaml-facing account pool config.
type AccountPoolConfig struct {
	Enabled             bool                    `yaml:"enabled"`
	MaxPreStreamRetries int                     `yaml:"max_prestream_retries"`
	CooldownSeconds     int                     `yaml:"cooldown_seconds_after_429"`
	ApplyToSubagents    bool                    `yaml:"apply_to_subagents"`
	Accounts            []AccountPoolAccountCfg `yaml:"accounts"`
}

// AccountPoolAccountCfg is the per-account yaml block.
type AccountPoolAccountCfg struct {
	Name          string `yaml:"name"`
	CredentialDir string `yaml:"credential_dir"`
	Priority      int    `yaml:"priority"`
}

// DefaultSMMConfig returns safe, fully-disabled defaults.
func DefaultSMMConfig() SMMConfig {
	return SMMConfig{
		Enabled:    false,
		StaticPlan: staticplan.DefaultConfig(),
	}
}

// smmHooks is the real (non-noop) Hooks implementation.
type smmHooks struct {
	pool    *accountpool.Pool
	planner *staticplan.Planner
	cfg     SMMConfig
}

// NewSMMHooks builds the real hooks implementation from SMMConfig.
// Returns DefaultHooks() (noop) when disabled or misconfigured.
func NewSMMHooks(cfg SMMConfig) Hooks {
	if !cfg.Enabled {
		return DefaultHooks()
	}

	// Build account pool.
	var pool *accountpool.Pool
	if cfg.AccountPool.Enabled && len(cfg.AccountPool.Accounts) > 0 {
		accRefs := make([]accountpool.AccountRef, 0, len(cfg.AccountPool.Accounts))
		for _, a := range cfg.AccountPool.Accounts {
			accRefs = append(accRefs, accountpool.AccountRef{
				Name:          a.Name,
				CredentialDir: a.CredentialDir,
				Priority:      a.Priority,
			})
		}
		pool = accountpool.NewPool(accountpool.Config{
			Enabled:             true,
			MaxPreStreamRetries: cfg.AccountPool.MaxPreStreamRetries,
			CooldownSeconds:     cfg.AccountPool.CooldownSeconds,
			Accounts:            accRefs,
		})
	}

	// Build static planner.
	planner := staticplan.NewPlanner(cfg.StaticPlan)

	if pool == nil && planner == nil {
		return DefaultHooks()
	}
	return &smmHooks{pool: pool, planner: planner, cfg: cfg}
}

// BeforeForward selects an account from the pool and injects its Bearer token
// into the outbound Authorization header. The selected AccountRef is stored in
// fc.SelectedAccount — never in a request header — so it is never forwarded to
// the upstream API.
func (h *smmHooks) BeforeForward(ctx context.Context, fc *ForwardContext) error {
	if h.pool == nil || (fc.ReqCtx.IsSubagent && !h.cfg.AccountPool.ApplyToSubagents) {
		return nil
	}
	meta := accountpool.RequestMeta{
		ThreadID:   fc.ReqCtx.ThreadID,
		SessionID:  fc.ReqCtx.SessionID,
		Model:      fc.ReqCtx.Model,
		IsSubagent: fc.ReqCtx.IsSubagent,
	}
	acc, err := h.pool.InjectAuthForAttempt(ctx, fc.OutboundReq, meta, fc.Attempt)
	if err != nil {
		log.Printf("[proxyext] BeforeForward: account injection failed (attempt=%d): %v", fc.Attempt, err)
		observability.RecordAuthFailure("<unknown>")
		return err
	}
	// Store selected account in context — never on the outbound request.
	fc.SelectedAccount = acc
	observability.RecordAccountSelected(acc.Name, fc.Attempt)
	return nil
}

// OnPreStreamResponse is called after upstream response headers are received
// but before any body bytes are written to the client. It uses the full
// AccountRef stored in fc.SelectedAccount (not a header reconstruction) to
// correctly update state and decide whether to retry.
func (h *smmHooks) OnPreStreamResponse(ctx context.Context, fc *ForwardContext, resp *http.Response) (RetryDecision, error) {
	if h.pool == nil {
		return RetryDecision{Retry: false}, nil
	}
	// fc.SelectedAccount is the full AccountRef set by BeforeForward.
	// streamStarted is always false here — this hook is called before flush.
	shouldRetry := h.pool.ShouldRetry(fc.SelectedAccount, resp, false, fc.Attempt)
	if shouldRetry {
		observability.RecordQuotaHit(fc.SelectedAccount.Name)
		return RetryDecision{
			Retry:       true,
			RetryReason: "pre_stream_quota",
			MaxAttempts: h.cfg.AccountPool.MaxPreStreamRetries,
		}, nil
	}
	return RetryDecision{Retry: false}, nil
}

// OnPostResponse records the final outcome for the selected account.
// Called after the stream ends or the non-streaming body is consumed.
func (h *smmHooks) OnPostResponse(_ context.Context, fc *ForwardContext, result ForwardResult) {
	if h.pool == nil {
		return
	}
	// If the stream was already in progress when it failed, the account
	// delivered bytes successfully — treat as success from the pool's view.
	if result.StreamStarted && result.ClassifiedFailure != "" {
		observability.RecordRetryBlockedAfterStream(fc.SelectedAccount.Name, result.StatusCode)
		h.pool.RecordSuccess(fc.SelectedAccount)
		return
	}
	if result.ClassifiedFailure == "" {
		h.pool.RecordSuccess(fc.SelectedAccount)
	}
}

func (h *smmHooks) TransformStaticPayload(_ context.Context, reqCtx RequestContext, ap *AssembledPrompt) error {
	if h.planner == nil || ap == nil || len(ap.RawBody) == 0 {
		return nil
	}
	plan := h.planner.Plan(ap.RawBody, reqCtx.IsSubagent)
	observability.RecordStaticPlan(string(plan.Mode), plan.Reason, !plan.Eligible)
	if !plan.Eligible {
		return nil
	}
	newBody, applied := h.planner.Apply(ap.RawBody, plan)
	if applied != "" {
		ap.RawBody = newBody
		ap.ContentHash = plan.ContentHash
		ap.TransformApplied = applied
	}
	return nil
}

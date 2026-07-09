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
//
// staticplan.NewPlanner is only called when StaticPlan.Enabled is true AND
// StaticPlan.Mode is not ModeOff. This ensures mode:off has zero allocation
// cost — the planner nil-check in TransformStaticPayload is the kill switch.
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

	// Build static planner only when explicitly enabled with a non-off mode.
	// mode:off or enabled:false → planner stays nil → TransformStaticPayload
	// returns immediately with zero allocations.
	var planner *staticplan.Planner
	if cfg.StaticPlan.Enabled && cfg.StaticPlan.Mode != staticplan.ModeOff {
		planner = staticplan.NewPlanner(cfg.StaticPlan)
	}

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
	// Store selected account in ForwardContext — never on the outbound request.
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

// TransformStaticPayload normalises or annotates the assembled prompt body
// before it is forwarded. All paths fail open: on any error, panic, or
// unexpected output, ap.RawBody is restored to its original value and the
// request continues untransformed.
//
// Fail-open guarantees:
//   - planner nil → immediate return, zero cost
//   - subagent && !ApplyToSubagents → immediate return, metric emitted
//   - Plan() returns !Eligible → immediate return, metric emitted
//   - Apply() returns empty applied string or zero-length body → restore
//     origBody, emit apply_noop_or_fail metric, return nil
//   - panic inside Plan() or Apply() → caught by dispatcher in hooks.go,
//     ap.RawBody is safe because assignment only occurs after nil/len checks
func (h *smmHooks) TransformStaticPayload(_ context.Context, reqCtx RequestContext, ap *AssembledPrompt) error {
	if h.planner == nil || ap == nil || len(ap.RawBody) == 0 {
		return nil
	}

	// Explicit subagent bypass — never rely on Plan() internals for this.
	if reqCtx.IsSubagent && !h.cfg.StaticPlan.ApplyToSubagents {
		observability.RecordStaticPlan("noop", "subagent_bypass", true)
		return nil
	}

	// Snapshot before any mutation. If Apply() produces bad output (empty
	// applied string or zero-length body), we restore and the request
	// continues with the original body. The panic recovery in the dispatcher
	// also relies on this: ap.RawBody is only overwritten after all checks
	// pass, so a mid-Apply panic leaves origBody as the safe fallback.
	origBody := ap.RawBody

	plan := h.planner.Plan(ap.RawBody, reqCtx.IsSubagent)
	observability.RecordStaticPlan(string(plan.Mode), plan.Reason, !plan.Eligible)
	if !plan.Eligible {
		return nil
	}

	newBody, applied := h.planner.Apply(ap.RawBody, plan)

	// Guard against silent failure: empty applied string OR zero-length body
	// means the transform either found nothing to do or failed internally.
	// Restore original and treat as noop — never forward a corrupted body.
	if applied == "" || len(newBody) == 0 {
		ap.RawBody = origBody
		observability.RecordStaticPlan(string(plan.Mode), "apply_noop_or_fail", true)
		return nil
	}

	ap.RawBody = newBody
	ap.ContentHash = plan.ContentHash
	ap.TransformApplied = applied
	return nil
}

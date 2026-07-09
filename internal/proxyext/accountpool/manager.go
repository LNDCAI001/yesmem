package accountpool

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// Manager is the top-level account pool coordinator.
// It wires together the selector, token provider, and retry logic,
// and implements proxyext.Extension for BeforeForward and OnPreStreamResponse.
type Manager struct {
	cfg      Config
	selector proxyext.AccountSelector
	tokens   proxyext.TokenProvider
	log      *slog.Logger

	// lastSelected is per-request state threaded through BeforeForward → OnPreStreamResponse.
	// Not safe for concurrent reuse across goroutines — each request needs its own Manager
	// or the caller must carry state externally. TODO: move to ForwardContext in v2.
	lastSelected proxyext.AccountRef
}

// NewManager constructs a Manager from config.
func NewManager(cfg Config, logger *slog.Logger) (*Manager, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("accountpool: not enabled")
	}
	if cfg.Provider != "claude_oauth" {
		return nil, fmt.Errorf("accountpool: unsupported provider %q (v1 supports claude_oauth only)", cfg.Provider)
	}
	sel, err := NewRoundRobinSelector(cfg)
	if err != nil {
		return nil, err
	}
	return &Manager{
		cfg:      cfg,
		select:   sel,
		tokens:   &OAuthStore{},
		log:      logger,
	}, nil
}

// BeforeForward selects an account and injects its OAuth token as the
// Authorization header on the outbound request.
//
// The request body is NOT touched. Thread/session headers are NOT touched.
// Only Authorization is replaced.
func (m *Manager) BeforeForward(ctx context.Context, fc *proxyext.ForwardContext) error {
	if fc.ReqCtx.IsSubagent && !m.cfg.ApplyToSubagents {
		return nil
	}

	account, err := m.selector.Select(ctx, fc)
	if err != nil {
		// All accounts unavailable — surface error; caller fails open.
		return fmt.Errorf("accountpool: %w", err)
	}
	m.lastSelected = account

	tok, err := m.tokens.GetAccessToken(ctx, account)
	if err != nil {
		m.log.Error("accountpool: token load failed",
			"account", account.Name,
			"error", err,
		)
		return fmt.Errorf("accountpool: token load for %q: %w", account.Name, err)
	}

	// Inject — never log the token value itself.
	fc.OutboundReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	m.log.Info("accountpool: selected account",
		"account", account.Name,
		"attempt", fc.Attempt,
	)
	return nil
}

// OnPreStreamResponse inspects the upstream response before WriteHeader.
// This is the ONLY point where account rotation retry is permitted.
func (m *Manager) OnPreStreamResponse(ctx context.Context, fc *proxyext.ForwardContext, resp *http.Response) (proxyext.RetryDecision, error) {
	if fc.ReqCtx.IsSubagent && !m.cfg.ApplyToSubagents {
		return proxyext.RetryDecision{}, nil
	}

	fc2 := FailureClass(Classify(resp, false /* stream not started yet */))

	if !IsRotatable(fc2) {
		// Not a quota/auth problem — let upstream handle it normally.
		if fc2 != FailureNone {
			m.log.Warn("accountpool: non-rotatable failure",
				"account", m.lastSelected.Name,
				"class", fc2,
				"status", resp.StatusCode,
			)
		}
		return proxyext.RetryDecision{}, nil
	}

	if fc.Attempt >= m.cfg.MaxPreStreamRetries {
		m.log.Warn("accountpool: retry limit reached",
			"account", m.lastSelected.Name,
			"attempt", fc.Attempt,
			"class", fc2,
		)
		return proxyext.RetryDecision{}, nil
	}

	// Mark this account limited before advancing.
	m.selector.MarkResult(proxyext.AccountResult{
		Account:           m.lastSelected,
		ClassifiedFailure: string(fc2),
		StreamStarted:     false,
		BytesFlushed:      false,
		Success:           false,
	})

	m.log.Info("accountpool: rotating account",
		"from", m.lastSelected.Name,
		"reason", fc2,
		"attempt", fc.Attempt,
	)

	return proxyext.RetryDecision{
		Retry:       true,
		RetryReason: string(fc2),
	}, nil
}

// OnPostResponse records success or post-stream failure for the selected account.
func (m *Manager) OnPostResponse(_ context.Context, fc *proxyext.ForwardContext, result proxyext.ForwardResult) {
	m.selector.MarkResult(proxyext.AccountResult{
		Account:           m.lastSelected,
		ClassifiedFailure: result.ClassifiedFailure,
		StreamStarted:     result.StreamStarted,
		BytesFlushed:      result.BytesFlushed,
		Success:           result.ClassifiedFailure == "",
	})
}

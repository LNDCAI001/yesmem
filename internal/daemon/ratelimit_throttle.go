package daemon

import (
	"encoding/json"
	"time"
)

// rateLimitWindow is the length of the subscription utilization window the
// snapshot describes. Older snapshots say nothing about current headroom.
const rateLimitWindow = 5 * time.Hour

// rateLimitThrottled reports whether background LLM work (extraction,
// narratives, persona) should pause because API utilization is above 50%.
//
// The snapshot is the last one the proxy persisted. A stale or undated
// snapshot must never throttle: when the proxy stopped persisting (the SMM
// path never did), a 99% reading from 2026-07-10 blocked extraction for months.
func rateLimitThrottled(rlJSON string, now time.Time) bool {
	if rlJSON == "" {
		return false
	}
	var rl struct {
		Unified5hUtilization float64   `json:"unified_5h_utilization"`
		Unified5hReset       time.Time `json:"unified_5h_reset"`
		TokensLimit          int       `json:"tokens_limit"`
		TokensRemaining      int       `json:"tokens_remaining"`
		IsSubscription       bool      `json:"is_subscription"`
		ParsedAt             time.Time `json:"parsed_at"`
	}
	if json.Unmarshal([]byte(rlJSON), &rl) != nil {
		return false
	}
	if rl.ParsedAt.IsZero() || now.Sub(rl.ParsedAt) > rateLimitWindow {
		return false
	}
	if !rl.Unified5hReset.IsZero() && now.After(rl.Unified5hReset) {
		return false
	}
	var utilization float64
	if rl.IsSubscription {
		utilization = rl.Unified5hUtilization
	} else if rl.TokensLimit > 0 {
		utilization = 1.0 - float64(rl.TokensRemaining)/float64(rl.TokensLimit)
	}
	return utilization > 0.5
}

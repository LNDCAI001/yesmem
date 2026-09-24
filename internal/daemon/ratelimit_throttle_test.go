package daemon

import (
	"fmt"
	"testing"
	"time"
)

func rlJSON(util float64, parsedAt time.Time) string {
	return fmt.Sprintf(`{"unified_5h_utilization":%v,"unified_5h_reset":"0001-01-01T00:00:00Z","is_subscription":true,"parsed_at":%q}`,
		util, parsedAt.Format(time.RFC3339Nano))
}

// Regression: a 99%% snapshot from 2026-07-10 stayed in proxy_state because the
// SMM proxy path never refreshed it, and extraction was throttled for months.
func TestRateLimitThrottled_IgnoresStaleSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 25, 6, 30, 0, 0, time.UTC)
	stale := rlJSON(0.99, now.Add(-76*24*time.Hour))
	if rateLimitThrottled(stale, now) {
		t.Fatal("a snapshot older than the 5h window must not throttle")
	}
}

func TestRateLimitThrottled_FreshHighUtilizationThrottles(t *testing.T) {
	now := time.Now()
	if !rateLimitThrottled(rlJSON(0.8, now.Add(-10*time.Minute)), now) {
		t.Fatal("fresh 80% utilization should throttle")
	}
	if rateLimitThrottled(rlJSON(0.3, now.Add(-10*time.Minute)), now) {
		t.Fatal("fresh 30% utilization should not throttle")
	}
}

func TestRateLimitThrottled_EmptyOrBrokenNeverThrottles(t *testing.T) {
	for _, in := range []string{"", "{", `{"unified_5h_utilization":0.9,"is_subscription":true}`} {
		if rateLimitThrottled(in, time.Now()) {
			t.Fatalf("input %q should not throttle", in)
		}
	}
}

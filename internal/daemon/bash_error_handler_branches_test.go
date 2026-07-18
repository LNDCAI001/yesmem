package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/models"
)

// TestTryStartAutoCorrect_GateBranches drives every refusal path of the
// rate-limiter plus the success path so a regression in any single gate
// produces a named subtest failure rather than blending into the larger
// processBashJobErrors flow tests.
func TestTryStartAutoCorrect_GateBranches(t *testing.T) {
	cap := "demo_branch"

	tests := []struct {
		name               string
		setup              func(t *testing.T, h *Handler)
		perTickCount       int
		wantProceed        bool
		wantReasonContains string
	}{
		{
			name:               "success on clean state",
			setup:              func(t *testing.T, h *Handler) {},
			perTickCount:       0,
			wantProceed:        true,
			wantReasonContains: "",
		},
		{
			name: "cooldown still active",
			setup: func(t *testing.T, h *Handler) {
				h.autoCorrectCooldown[cap] = time.Now().Add(time.Minute)
			},
			perTickCount:       0,
			wantProceed:        false,
			wantReasonContains: "cooldown until",
		},
		{
			name: "generation limit reached",
			setup: func(t *testing.T, h *Handler) {
				now := time.Now()
				for i := 0; i < autoCorrectMaxGenerationsPerDay; i++ {
					if _, err := h.store.InsertLearning(&models.Learning{
						Category:    "cap_proposed",
						Content:     fmt.Sprintf("seeded gen %d", i),
						Source:      "auto_correct_proposal",
						TriggerRule: "cap_proposed:" + cap,
						CreatedAt:   now,
					}); err != nil {
						t.Fatalf("seed gen-limit row %d: %v", i, err)
					}
				}
			},
			perTickCount:       0,
			wantProceed:        false,
			wantReasonContains: "generation limit",
		},
		{
			name:               "per-tick budget exhausted",
			setup:              func(t *testing.T, h *Handler) {},
			perTickCount:       autoCorrectMaxPerTick,
			wantProceed:        false,
			wantReasonContains: "per-tick budget",
		},
		{
			name: "another auto-correct in flight",
			setup: func(t *testing.T, h *Handler) {
				h.autoCorrectRunning = true
			},
			perTickCount:       0,
			wantProceed:        false,
			wantReasonContains: "in flight",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := mustHandler(t)
			tc.setup(t, h)

			proceed, reason := h.tryStartAutoCorrect(cap, tc.perTickCount)

			if proceed != tc.wantProceed {
				t.Errorf("proceed=%v reason=%q, want proceed=%v", proceed, reason, tc.wantProceed)
			}
			if tc.wantReasonContains != "" && !strings.Contains(reason, tc.wantReasonContains) {
				t.Errorf("reason=%q must contain %q", reason, tc.wantReasonContains)
			}
			if tc.wantProceed {
				if !h.autoCorrectRunning {
					t.Errorf("success path must set autoCorrectRunning=true")
				}
				if _, ok := h.autoCorrectCooldown[cap]; !ok {
					t.Errorf("success path must seed cooldown timestamp for %q", cap)
				}
			}
		})
	}
}

package daemon

import (
	"encoding/json"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/models"
)

// TestIntegration_OriginEndToEnd verifies that handleRemember correctly persists
// origin_tool for three representative provenance labels, and that
// models.OriginMultiplier returns a value in (0, 1] for each.
func TestIntegration_OriginEndToEnd(t *testing.T) {
	cases := []struct {
		name           string
		params         map[string]any
		wantOriginTool string
	}{
		{
			name:           "default user origin",
			params:         map[string]any{"text": "user note"},
			wantOriginTool: "user",
		},
		{
			name:           "web_external origin",
			params:         map[string]any{"text": "web note", "origin": "web_external"},
			wantOriginTool: "web_external",
		},
		{
			name:           "bash_command_input origin",
			params:         map[string]any{"text": "bash note", "origin": "bash_command_input"},
			wantOriginTool: "bash_command_input",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, s := mustHandler(t)

			resp := h.Handle(Request{
				Method: "remember",
				Params: tc.params,
			})
			if resp.Error != "" {
				t.Fatalf("handleRemember error: %s", resp.Error)
			}

			var result map[string]any
			if err := json.Unmarshal(resp.Result, &result); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}

			id, ok := result["id"].(float64)
			if !ok || id <= 0 {
				t.Fatalf("invalid id in response: %v", result["id"])
			}

			l, err := s.GetLearning(int64(id))
			if err != nil {
				t.Fatalf("GetLearning(%d): %v", int64(id), err)
			}

			if l.OriginTool != tc.wantOriginTool {
				t.Errorf("OriginTool = %q, want %q", l.OriginTool, tc.wantOriginTool)
			}

			m := models.OriginMultiplier(l.OriginTool)
			if m <= 0 || m > 1 {
				t.Errorf("OriginMultiplier(%q) = %.2f, want in (0, 1]", l.OriginTool, m)
			}
		})
	}
}

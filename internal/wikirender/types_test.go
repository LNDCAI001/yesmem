package wikirender

import (
	"strings"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/storage"
)

func TestRenderConfig_Validate(t *testing.T) {
	cases := []struct {
		name string
		cfg  RenderConfig
		err  string
	}{
		{"empty", RenderConfig{}, "Project is required"},
		{"no-out", RenderConfig{Project: "p"}, "OutputDir is required"},
		{"no-store", RenderConfig{Project: "p", OutputDir: "/tmp/x"}, "Store is required"},
		{"valid", RenderConfig{Project: "p", OutputDir: "/tmp/x", Store: &storage.Store{}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.err == "" {
				if err != nil {
					t.Errorf("expected nil, got %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("expected %q, got %v", tc.err, err)
				}
			}
		})
	}
}

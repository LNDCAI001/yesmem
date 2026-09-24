package extraction

import (
	"strings"
	"testing"
)

// Regression (daemon.log 2026-09-25): extraction calls ran the user settings.json
// hooks (yesmem briefing-hook, LifeOS SessionStart/SessionEnd), injecting large
// context into every call and burning subscription usage. --safe-mode skips hooks,
// CLAUDE.md, skills and MCP while keeping OAuth auth (unlike --bare, which needs an API key).
func TestBuildCLIScript_SkipsUserCustomizations(t *testing.T) {
	s := buildCLIScript("claude", "sonnet", "/tmp/sys.txt", "", "/tmp/msg.txt")
	for _, want := range []string{"--safe-mode", "--tools=", "--strict-mcp-config", "--no-session-persistence", "unset ANTHROPIC_BASE_URL"} {
		if !strings.Contains(s, want) {
			t.Errorf("wrapper script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "--bare") {
		t.Error("--bare requires ANTHROPIC_API_KEY and breaks OAuth subscription auth")
	}
}

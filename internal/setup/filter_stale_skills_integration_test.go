package setup

import (
	"os"
	"strings"
	"testing"
)

func TestFilterStaleSkillRules_RealRULESmd(t *testing.T) {
	data, err := os.ReadFile("../../RULES.md")
	if err != nil {
		t.Skipf("RULES.md not found: %v", err)
	}

	available := loadAgentLoadableSkillNames()
	t.Logf("available skills: %d", len(available))

	filtered := filterStaleSkillRulesWith(string(data), available)

	// Skills NOT available on this machine must be filtered from the catalog
	unavailable := []string{"yesmem-docs", "yesmem-orientation", "yesmem-planning", "yesmem-search", "yesmem-sessions"}
	for _, skill := range unavailable {
		if strings.Contains(filtered, "skill: "+skill) {
			t.Errorf("unavailable skill %q still present in catalog after filtering", skill)
		}
	}

	// Skills that ARE available AND were in the original catalog must survive
	catalogSkillsThatShouldSurvive := []string{"yesmem-config", "yesmem-agents", "yesmem-cap-builder", "yesmem-remember"}
	for _, skill := range catalogSkillsThatShouldSurvive {
		if !strings.Contains(filtered, "skill: "+skill) {
			t.Errorf("available catalog skill %q was incorrectly filtered out", skill)
		}
	}
}

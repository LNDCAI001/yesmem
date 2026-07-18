package briefing

import (
	"strings"
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/models"
)

func TestRenderCapabilities_WithCapabilities(t *testing.T) {
	g := &Generator{maxPerCategory: 5, strings: DefaultStrings()}
	learnings := []models.Learning{
		{Category: "cap", Content: "reddit_fetch — Fetch Reddit posts", Keywords: []string{"web", "reddit"}, CreatedAt: time.Now()},
		{Category: "cap", Content: "hn_search — Search Hacker News", Keywords: []string{"web", "hn"}, CreatedAt: time.Now()},
		{Category: "gotcha", Content: "unrelated gotcha", CreatedAt: time.Now()},
	}
	result := g.renderCaps(g.strings, learnings)
	if !strings.Contains(result, "reddit_fetch") {
		t.Error("should contain reddit_fetch")
	}
	if !strings.Contains(result, "hn_search") {
		t.Error("should contain hn_search")
	}
	if !strings.Contains(result, "(web, reddit)") {
		t.Error("should contain tags for reddit_fetch")
	}
	if strings.Contains(result, "unrelated gotcha") {
		t.Error("should NOT contain non-capability learnings")
	}
	if !strings.Contains(result, "get_caps()") {
		t.Error("should contain hint to use get_caps()")
	}
}

func TestRenderCapabilities_Empty(t *testing.T) {
	g := &Generator{maxPerCategory: 5, strings: DefaultStrings()}
	learnings := []models.Learning{
		{Category: "gotcha", Content: "some gotcha", CreatedAt: time.Now()},
	}
	result := g.renderCaps(g.strings, learnings)
	if result != "" {
		t.Errorf("expected empty string for no capabilities, got: %q", result)
	}
}

func TestRenderCapabilities_NoKeywords(t *testing.T) {
	g := &Generator{maxPerCategory: 5, strings: DefaultStrings()}
	learnings := []models.Learning{
		{Category: "cap", Content: "simple_tool — A simple tool", CreatedAt: time.Now()},
	}
	result := g.renderCaps(g.strings, learnings)
	if !strings.Contains(result, "simple_tool — A simple tool") {
		t.Error("should contain simple_tool line without tags")
	}
	// Should NOT have tags appended to the item line
	if strings.Contains(result, "simple_tool — A simple tool (") {
		t.Error("should not have tag parens for no keywords")
	}
}

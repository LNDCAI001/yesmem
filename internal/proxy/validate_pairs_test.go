package proxy

import (
	"testing"
)

func TestValidateToolPairs_NoOrphans(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu_1", "name": "read"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "file contents"},
		}},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 0 {
		t.Errorf("expected 0 orphans, got %d", orphans)
	}
	if len(result) != 3 {
		t.Errorf("expected 3 messages, got %d", len(result))
	}
}

func TestValidateToolPairs_SingleOrphan(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_missing", "content": "orphan"},
			map[string]any{"type": "text", "text": "keep this"},
		}},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 1 {
		t.Errorf("expected 1 orphan, got %d", orphans)
	}
	if len(result) != 3 {
		t.Errorf("expected 3 messages, got %d", len(result))
	}
	// The remaining message should have only the text block
	msg := result[2].(map[string]any)
	content := msg["content"].([]any)
	if len(content) != 1 {
		t.Errorf("expected 1 block after orphan removal, got %d", len(content))
	}
}

func TestValidateToolPairs_OrphanOnlyMessage(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "thinking..."},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_gone", "content": "orphan"},
		}},
		map[string]any{"role": "assistant", "content": "response"},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 1 {
		t.Errorf("expected 1 orphan, got %d", orphans)
	}
	// Message 2 (orphan-only) removed, messages 1+3 both assistant → merged
	// Result: user, assistant (merged)
	if len(result) != 2 {
		t.Errorf("expected 2 messages after removal+merge, got %d", len(result))
	}
}

func TestValidateToolPairs_MultipleOrphans(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu_1", "name": "read"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "valid"},
			map[string]any{"type": "tool_result", "tool_use_id": "tu_dead1", "content": "orphan1"},
			map[string]any{"type": "tool_result", "tool_use_id": "tu_dead2", "content": "orphan2"},
		}},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 2 {
		t.Errorf("expected 2 orphans, got %d", orphans)
	}
	msg := result[2].(map[string]any)
	content := msg["content"].([]any)
	if len(content) != 1 {
		t.Errorf("expected 1 block remaining, got %d", len(content))
	}
}

func TestValidateToolPairs_StringContent(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "just text"},
		map[string]any{"role": "assistant", "content": "also text"},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 0 {
		t.Errorf("expected 0 orphans, got %d", orphans)
	}
	if len(result) != 2 {
		t.Errorf("expected 2 messages, got %d", len(result))
	}
}

func TestValidateToolPairs_EmptySlice(t *testing.T) {
	result, orphans := validateToolPairs([]any{}, nil)
	if orphans != 0 || len(result) != 0 {
		t.Errorf("expected empty result, got %d messages %d orphans", len(result), orphans)
	}
}

func TestValidateToolPairs_MixedValidAndOrphan(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu_valid", "name": "bash"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_valid", "content": "ok"},
			map[string]any{"type": "tool_result", "tool_use_id": "tu_orphan", "content": "nope"},
		}},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 1 {
		t.Errorf("expected 1 orphan, got %d", orphans)
	}
	msg := result[2].(map[string]any)
	content := msg["content"].([]any)
	if len(content) != 1 {
		t.Errorf("expected 1 valid block, got %d", len(content))
	}
	block := content[0].(map[string]any)
	if block["tool_use_id"] != "tu_valid" {
		t.Errorf("expected tu_valid to survive, got %v", block["tool_use_id"])
	}
}

// countSurvivingToolUses walks the repaired messages and returns how many tool_use
// blocks survive — used to assert orphan tool_use removal.
func countSurvivingToolUses(messages []any) int {
	n := 0
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range content {
			if b, ok := block.(map[string]any); ok && b["type"] == "tool_use" {
				n++
			}
		}
	}
	return n
}

// TestValidateToolPairs_OrphanToolUse is the direct regression test for the
// 400 "tool use concurrency" fix: an assistant tool_use with no matching
// tool_result (e.g. its result was dropped by collapse) must be removed.
func TestValidateToolPairs_OrphanToolUse(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu_orphan", "name": "read"},
		}},
		map[string]any{"role": "user", "content": "next question"},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 1 {
		t.Fatalf("expected 1 orphan tool_use removed, got %d", orphans)
	}
	if got := countSurvivingToolUses(result); got != 0 {
		t.Fatalf("expected orphan tool_use gone, still have %d", got)
	}
}

// TestValidateToolPairs_OrphanToolUseKeepsText verifies only the orphan
// tool_use is stripped, leaving sibling text in the same message intact.
func TestValidateToolPairs_OrphanToolUseKeepsText(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "let me check"},
			map[string]any{"type": "tool_use", "id": "tu_orphan", "name": "bash"},
		}},
		map[string]any{"role": "user", "content": "ok"},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 1 {
		t.Fatalf("expected 1 orphan, got %d", orphans)
	}
	msg := result[1].(map[string]any)
	content := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("expected only the text block to survive, got %v", content)
	}
}

// TestValidateToolPairs_ValidPairSurvives is a regression guard: a tool_use
// WITH its matching tool_result must never be treated as an orphan.
func TestValidateToolPairs_ValidPairSurvives(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu_1", "name": "read"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "ok"},
		}},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 0 {
		t.Fatalf("valid tool_use/tool_result pair must not be removed, got %d orphans", orphans)
	}
	if got := countSurvivingToolUses(result); got != 1 {
		t.Fatalf("expected the paired tool_use to survive, got %d", got)
	}
}

// TestValidateToolPairs_BothDirections removes an orphan tool_use and an orphan
// tool_result in one request while preserving the fully-paired pair.
func TestValidateToolPairs_BothDirections(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "tu_paired", "name": "read"},
			map[string]any{"type": "tool_use", "id": "tu_orphanuse", "name": "read"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu_paired", "content": "ok"},
			map[string]any{"type": "tool_result", "tool_use_id": "tu_orphanres", "content": "nope"},
		}},
	}

	result, orphans := validateToolPairs(messages, nil)
	if orphans != 2 {
		t.Fatalf("expected 2 orphans (1 use + 1 result), got %d", orphans)
	}
	if got := countSurvivingToolUses(result); got != 1 {
		t.Fatalf("expected only the paired tool_use to survive, got %d", got)
	}
}

package extraction

import (
	"testing"

	"github.com/LNDCAI001/yesmem/internal/models"
)

// TestParseExtractionResponse_OriginTool asserts that learnings produced by
// parseExtractionResponse carry OriginTool == "llm_extracted_session".
func TestParseExtractionResponse_OriginTool(t *testing.T) {
	response := `{
		"domain": "code",
		"learnings": [
			{"category": "gotcha", "content": "Port 8080 belegt — Wechsel auf 8081", "context": "", "entities": [], "actions": [], "keywords": [], "trigger": "", "importance": 3}
		],
		"session_emotional_intensity": 0.3,
		"session_flavor": "test"
	}`

	learnings, err := parseExtractionResponse(response, "s-origin-1", "haiku")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(learnings) == 0 {
		t.Fatal("expected at least one learning")
	}
	for _, l := range learnings {
		if l.OriginTool != "llm_extracted_session" {
			t.Errorf("parseExtractionResponse: OriginTool = %q, want %q", l.OriginTool, "llm_extracted_session")
		}
	}
}

// TestExtractFromSession_OriginTool asserts that learnings produced by the
// full two-pass extractor (addLearningsV2 path) carry OriginTool == "llm_extracted_session".
func TestExtractFromSession_OriginTool(t *testing.T) {
	summarizeClient := &mockLLMClient{
		completeFunc: func(system, user string) (string, error) {
			return "User entschied Go statt Rust. Ruhige Session.", nil
		},
		model: "haiku",
	}
	extractClient := &mockLLMClient{
		completeJSONFunc: func(system, user string, schema map[string]any) (string, error) {
			return `{
				"domain": "code",
				"learnings": [
					{"category": "decision", "content": "Go statt Rust weil pure Go SQLite", "context": "", "entities": [], "actions": [], "keywords": [], "trigger": "", "importance": 4}
				],
				"session_emotional_intensity": 0.2,
				"session_flavor": "Ruhige Entscheidungs-Session"
			}`, nil
		},
		model: "sonnet",
	}

	ext := NewTwoPassExtractor(summarizeClient, extractClient, nil)

	msgs := []models.Message{
		{Role: "user", MessageType: "text", Content: "Go oder Rust?"},
		{Role: "assistant", MessageType: "text", Content: "Go — pure SQLite ohne CGO."},
	}

	learnings, err := ext.ExtractFromSession("s-origin-2", msgs)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(learnings) == 0 {
		t.Fatal("expected at least one learning")
	}
	for _, l := range learnings {
		if l.OriginTool != "llm_extracted_session" {
			t.Errorf("ExtractFromSession: OriginTool = %q, want %q", l.OriginTool, "llm_extracted_session")
		}
	}
}

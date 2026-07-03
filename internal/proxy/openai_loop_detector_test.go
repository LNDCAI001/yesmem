package proxy

import (
	"io"
	"log"
	"strings"
	"testing"
)

// TestOpenAIPipeline_LoopDetectionFires ensures CheckLoopAndFormat runs on the
// OpenAI-parity path so opencode-requests get loop warnings. Previously the
// warning was only wired into handleMessages (Anthropic path), so opencode
// sessions looping on identical tool calls got zero warnings.
func TestOpenAIPipeline_LoopDetectionFires(t *testing.T) {
	// 2 identical Edit→Bash cycles in OpenAI format. After translation these
	// become the canonical Anthropic tool_use/tool_result pattern that
	// detectIdenticalCycle triggers on.
	oaiReq := OpenAIChatRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "fix it"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_2", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_2", Content: "ok"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_3", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_3", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_4", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_4", Content: "ok"},
		},
	}

	anthReq, err := translateOpenAIToAnthropic(oaiReq)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := &Server{
		cfg: Config{
			TokenThreshold:        200000,
			TokenMinimumThreshold: 80000,
			SawtoothEnabled:       false,
			SkillEvalInject:       "false",
		},
		logger:     log.New(io.Discard, "", 0),
		loopStates: map[string]*LoopState{},
	}

	ctx := openAIRequestContext{
		ReqIdx:   1,
		ThreadID: "opencode:test-session",
		Project:  "test",
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if !hasLoopWarning(anthReq) {
		t.Errorf("expected yesmem-loop-warning system block after OpenAI pipeline; got system blocks: %+v", anthReq["system"])
	}
}

// TestOpenAIPipeline_LoopDetection_RetrySkipped verifies the gate: retry
// requests (ctx.Retry=true) must not run loop detection, mirroring the
// Anthropic path's !isRetryReq guard.
func TestOpenAIPipeline_LoopDetection_RetrySkipped(t *testing.T) {
	oaiReq := OpenAIChatRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "fix it"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_2", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_2", Content: "ok"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_3", Type: "function", Function: OpenAIFunctionCall{
					Name: "Edit", Arguments: `{"file_path":"main.go","old_string":"a","new_string":"b"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_3", Content: "edited"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_4", Type: "function", Function: OpenAIFunctionCall{
					Name: "Bash", Arguments: `{"command":"go test"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_4", Content: "ok"},
		},
	}

	anthReq, err := translateOpenAIToAnthropic(oaiReq)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := &Server{
		cfg: Config{
			TokenThreshold:        200000,
			TokenMinimumThreshold: 80000,
			SawtoothEnabled:       false,
			SkillEvalInject:       "false",
		},
		logger:     log.New(io.Discard, "", 0),
		loopStates: map[string]*LoopState{},
	}

	ctx := openAIRequestContext{
		ReqIdx:   1,
		ThreadID: "opencode:test-session",
		Project:  "test",
		Retry:    true,
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if hasLoopWarning(anthReq) {
		t.Errorf("retry request must NOT inject loop warning; got system blocks: %+v", anthReq["system"])
	}
}

// TestOpenAIPipeline_LoopDetection_NoLoopNoWarning verifies that a single
// tool call (no loop pattern) does not inject a warning — guards against
// false positives.
func TestOpenAIPipeline_LoopDetection_NoLoopNoWarning(t *testing.T) {
	oaiReq := OpenAIChatRequest{
		Model:     "glm-5.2",
		MaxTokens: 1024,
		Messages: []OpenAIMessage{
			{Role: "user", Content: "read the file"},
			{Role: "assistant", Content: "", ToolCalls: []OpenAIToolCall{
				{ID: "call_1", Type: "function", Function: OpenAIFunctionCall{
					Name: "Read", Arguments: `{"file_path":"main.go"}`,
				}},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "package main"},
		},
	}

	anthReq, err := translateOpenAIToAnthropic(oaiReq)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	s := &Server{
		cfg: Config{
			TokenThreshold:        200000,
			TokenMinimumThreshold: 80000,
			SawtoothEnabled:       false,
			SkillEvalInject:       "false",
		},
		logger:     log.New(io.Discard, "", 0),
		loopStates: map[string]*LoopState{},
	}

	ctx := openAIRequestContext{
		ReqIdx:   1,
		ThreadID: "opencode:test-session",
		Project:  "test",
	}

	s.runOpenAIParityPipeline(anthReq, &ctx)

	if hasLoopWarning(anthReq) {
		t.Errorf("single tool call must not trigger loop warning; got system blocks: %+v", anthReq["system"])
	}
}

func hasLoopWarning(req map[string]any) bool {
	blocks, _ := req["system"].([]any)
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		text, _ := bm["text"].(string)
		if strings.Contains(text, "yesmem-loop-warning") ||
			strings.Contains(text, "[YesMem Loop Detection]") {
			return true
		}
	}
	return false
}

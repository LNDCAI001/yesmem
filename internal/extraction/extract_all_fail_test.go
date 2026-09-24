package extraction

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/models"
)

func sampleMsgs() []models.Message {
	now := time.Now()
	return []models.Message{
		{Role: "user", MessageType: "text", Content: strings.Repeat("please fix the build ", 20), Timestamp: now},
		{Role: "assistant", MessageType: "text", Content: strings.Repeat("fixed the go build by pinning the toolchain ", 20), Timestamp: now},
	}
}

// Regression (daemon.log 2026-09-25 06:48): every CLI call failed (expired
// OAuth login) yet ExtractFromSession returned nil, so the daemon marked 30
// sessions "extracted" with 0 learnings and they were never retried.
func TestExtractFromSession_AllChunksFailReturnsError(t *testing.T) {
	c := &mockLLMClient{completeJSONFunc: func(string, string, map[string]any) (string, error) {
		return "", errors.New("cli error: exit status 1")
	}}
	if _, err := NewExtractor(c, nil).ExtractFromSession("s1", sampleMsgs()); err == nil {
		t.Fatal("all LLM calls failed: expected an error so the session is not marked extracted")
	}
}

func TestExtractFromSession_EmptySuccessIsNotAnError(t *testing.T) {
	c := &mockLLMClient{completeJSONFunc: func(string, string, map[string]any) (string, error) {
		return `{"domain":"code","learnings":[]}`, nil
	}}
	if _, err := NewExtractor(c, nil).ExtractFromSession("s1", sampleMsgs()); err != nil {
		t.Fatalf("a successful call with zero learnings is valid, got %v", err)
	}
}

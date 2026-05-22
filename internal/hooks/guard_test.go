package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeFile is a test helper to write a file or panic.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestFormatGuardOutput_PASS(t *testing.T) {
	out := formatGuardOutput(GuardDecision{Decision: "PASS"})
	if out != "" {
		t.Errorf("PASS should be silent, got %q", out)
	}
}

func TestFormatGuardOutput_SUGGEST(t *testing.T) {
	out := formatGuardOutput(GuardDecision{
		Decision:   "SUGGEST",
		Suggestion: "test-driven-development: write tests first",
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid json: %v (out=%s)", err, out)
	}
	hso, ok := parsed["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput, got %v", parsed)
	}
	if hso["hookEventName"] != "PreToolUse" {
		t.Errorf("expected hookEventName=PreToolUse, got %v", hso["hookEventName"])
	}
	if hso["additionalContext"] != "test-driven-development: write tests first" {
		t.Errorf("additionalContext mismatch, got %v", hso["additionalContext"])
	}
}

func TestFormatGuardOutput_SUGGEST_EmptyIsSilent(t *testing.T) {
	out := formatGuardOutput(GuardDecision{Decision: "SUGGEST", Suggestion: ""})
	if out != "" {
		t.Errorf("SUGGEST without suggestion should be silent, got %q", out)
	}
}

func TestFormatGuardOutput_BLOCK(t *testing.T) {
	out := formatGuardOutput(GuardDecision{
		Decision:   "BLOCK",
		Violations: []string{"No Claude signature in commits", "rule 4"},
	})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid json: %v (out=%s)", err, out)
	}
	if parsed["decision"] != "block" {
		t.Errorf("expected decision=block, got %v", parsed["decision"])
	}
	reason, _ := parsed["reason"].(string)
	if !strings.Contains(reason, "Claude signature") || !strings.Contains(reason, "rule 4") {
		t.Errorf("reason should join violations, got %q", reason)
	}
	hso, ok := parsed["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput, got %v", parsed)
	}
	if hso["additionalContext"] != reason {
		t.Errorf("additionalContext should mirror reason, got %v", hso["additionalContext"])
	}
}

func TestFormatGuardOutput_BLOCK_NoViolations(t *testing.T) {
	out := formatGuardOutput(GuardDecision{Decision: "BLOCK"})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid json: %v (out=%s)", err, out)
	}
	if parsed["decision"] != "block" {
		t.Errorf("expected decision=block, got %v", parsed["decision"])
	}
	if reason, _ := parsed["reason"].(string); reason == "" {
		t.Error("reason should fall back to default text when violations are empty")
	}
}

func TestResolveGuardConfig_UsesModelFromConfig(t *testing.T) {
	td := t.TempDir()
	// Override HOME so resolveGuardConfig reads from our temp dir
	t.Setenv("HOME", td)

	// Create config.yaml with custom model
	writeFile(t, filepath.Join(td, ".claude", "yesmem", "config.yaml"),
		"extraction:\n  model: deepseek-v4-pro\n")

	// Create models.json
	writeFile(t, filepath.Join(td, ".cache", "opencode", "models.json"),
		`{"deepseek":{"api":"https://api.deepseek.com","models":{"deepseek-v4-pro":{}}}}`)

	// Create auth.json
	writeFile(t, filepath.Join(td, ".local", "share", "opencode", "auth.json"),
		`{"deepseek":{"key":"sk-test123"}}`)

	dataDir := filepath.Join(td, ".claude", "yesmem")
	cfg, err := resolveGuardConfig(dataDir)
	if err != nil {
		t.Fatalf("resolveGuardConfig: %v", err)
	}
	if cfg.Model != "deepseek-v4-pro" {
		t.Errorf("expected model deepseek-v4-pro, got %s", cfg.Model)
	}
	if cfg.APIKey != "sk-test123" {
		t.Errorf("expected API key sk-test123, got %s", cfg.APIKey)
	}
	if !strings.Contains(cfg.APIURL, "api.deepseek.com") {
		t.Errorf("expected URL containing api.deepseek.com, got %s", cfg.APIURL)
	}
}

func TestResolveGuardConfig_PrefersProviderWithKey(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)

	// Config with default model
	writeFile(t, filepath.Join(td, ".claude", "yesmem", "config.yaml"),
		"extraction:\n  model: deepseek-v4-flash\n")

	// Two providers: auriko first (no key), deepseek second (has key)
	writeFile(t, filepath.Join(td, ".cache", "opencode", "models.json"),
		`{
			"auriko":{"api":"https://api.auriko.ai/v1","models":{"deepseek-v4-flash":{}}},
			"deepseek":{"api":"https://api.deepseek.com","models":{"deepseek-v4-flash":{}}}
		}`)

	writeFile(t, filepath.Join(td, ".local", "share", "opencode", "auth.json"),
		`{"deepseek":{"key":"sk-test123"}}`)

	dataDir := filepath.Join(td, ".claude", "yesmem")
	cfg, err := resolveGuardConfig(dataDir)
	if err != nil {
		t.Fatalf("resolveGuardConfig: %v", err)
	}
	if !strings.Contains(cfg.APIURL, "deepseek.com") {
		t.Errorf("expected deepseek URL (provider with key), got %s", cfg.APIURL)
	}
	if cfg.APIKey != "sk-test123" {
		t.Errorf("expected api key from deepseek, got %s", cfg.APIKey)
	}
}

func TestResolveOpenCodeConfig_FallsBackToProviderWithoutKey(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)

	writeFile(t, filepath.Join(td, ".claude", "yesmem", "config.yaml"),
		"extraction:\n  model: deepseek-v4-flash\n")

	// Only auriko, no auth key
	writeFile(t, filepath.Join(td, ".cache", "opencode", "models.json"),
		`{"auriko":{"api":"https://api.auriko.ai/v1","models":{"deepseek-v4-flash":{}}}}`)

	// Empty auth
	writeFile(t, filepath.Join(td, ".local", "share", "opencode", "auth.json"),
		`{}`)

	dataDir := filepath.Join(td, ".claude", "yesmem")
	cfg, err := resolveOpenCodeConfig(dataDir)
	if err != nil {
		t.Fatalf("resolveOpenCodeConfig: %v", err)
	}
	if !strings.Contains(cfg.APIURL, "auriko") {
		t.Errorf("expected fallback to auriko URL, got %s", cfg.APIURL)
	}
	if cfg.APIKey != "" {
		t.Errorf("expected empty API key for fallback provider, got %s", cfg.APIKey)
	}
}

func TestResolveGuardConfig_FallsBackToAnthropic(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)

	// OpenCode config missing entirely — only create a config.yaml (which fails because no models.json)
	writeFile(t, filepath.Join(td, ".claude", "yesmem", "config.yaml"),
		"extraction:\n  model: deepseek-v4-flash\n")

	// But set ANTHROPIC_API_KEY in env
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fallback")

	dataDir := filepath.Join(td, ".claude", "yesmem")
	cfg, err := resolveGuardConfig(dataDir)
	if err != nil {
		t.Fatalf("resolveGuardConfig: %v", err)
	}
	if cfg.APIType != "anthropic" {
		t.Errorf("expected anthropic api type, got %s", cfg.APIType)
	}
	if cfg.APIKey != "sk-ant-fallback" {
		t.Errorf("expected Anthropic API key, got %s", cfg.APIKey)
	}
	if !strings.Contains(cfg.APIURL, "anthropic.com") {
		t.Errorf("expected anthropic URL, got %s", cfg.APIURL)
	}
}

func TestResolveGuardConfig_OpenCodeTakesPriority(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-be-used")

	// OpenCode has everything it needs
	writeFile(t, filepath.Join(td, ".claude", "yesmem", "config.yaml"),
		"extraction:\n  model: deepseek-v4-flash\n")
	writeFile(t, filepath.Join(td, ".cache", "opencode", "models.json"),
		`{"deepseek":{"api":"https://api.deepseek.com","models":{"deepseek-v4-flash":{}}}}`)
	writeFile(t, filepath.Join(td, ".local", "share", "opencode", "auth.json"),
		`{"deepseek":{"key":"sk-opencode"}}`)

	dataDir := filepath.Join(td, ".claude", "yesmem")
	cfg, err := resolveGuardConfig(dataDir)
	if err != nil {
		t.Fatalf("resolveGuardConfig: %v", err)
	}
	if cfg.APIType != "opencode" {
		t.Errorf("expected opencode api type, got %s", cfg.APIType)
	}
	if cfg.APIKey != "sk-opencode" {
		t.Errorf("expected opencode API key, got %s", cfg.APIKey)
	}
}

func TestResolveAnthropicConfig_FromEnvVar(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-env-test")
	t.Setenv("HOME", "/nonexistent")

	cfg, err := resolveAnthropicConfig()
	if err != nil {
		t.Fatalf("resolveAnthropicConfig: %v", err)
	}
	if cfg.APIKey != "sk-ant-env-test" {
		t.Errorf("expected env var key, got %s", cfg.APIKey)
	}
	if cfg.Model != "claude-3-haiku-20240307" {
		t.Errorf("expected haiku model, got %s", cfg.Model)
	}
}

func TestResolveAnthropicConfig_FromClaudeConfig(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("ANTHROPIC_API_KEY", "") // clear env

	writeFile(t, filepath.Join(td, ".claude", "config.json"),
		`{"primaryApiKey":"sk-ant-config-file"}`)

	cfg, err := resolveAnthropicConfig()
	if err != nil {
		t.Fatalf("resolveAnthropicConfig: %v", err)
	}
	if cfg.APIKey != "sk-ant-config-file" {
		t.Errorf("expected config file key, got %s", cfg.APIKey)
	}
}

func TestResolveAnthropicConfig_FromClaudeDotJSON(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("ANTHROPIC_API_KEY", "")

	writeFile(t, filepath.Join(td, ".claude.json"),
		`{"primaryApiKey":"sk-ant-dotjson"}`)

	cfg, err := resolveAnthropicConfig()
	if err != nil {
		t.Fatalf("resolveAnthropicConfig: %v", err)
	}
	if cfg.APIKey != "sk-ant-dotjson" {
		t.Errorf("expected ~/.claude.json key, got %s", cfg.APIKey)
	}
}

func TestResolveAnthropicConfig_NotFound(t *testing.T) {
	td := t.TempDir()
	t.Setenv("HOME", td)
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := resolveAnthropicConfig()
	if err == nil {
		t.Fatal("expected error when no key found, got nil")
	}
}

func TestEvaluateGuard_ParsesJSON(t *testing.T) {
	// Reset guard cache for clean test
	guardCacheMu.Lock()
	guardCache = make(map[string]guardCacheEntry)
	guardCacheMu.Unlock()

	// Start a test HTTP server that returns a valid decision
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request has correct headers
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Error("missing or wrong Authorization header")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing Content-Type header")
		}
		// Return a SUGGEST decision
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"content": `{"decision":"SUGGEST","suggestion":"test-skill: use test pattern"}`,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &guardConfig{
		Model:  "deepseek-v4-flash",
		APIURL: server.URL + "/v1/chat/completions",
		APIKey: "sk-test",
	}

	decision := evaluateGuard(cfg, "Some rules content", "Bash: git push", "Bash")
	if decision.Decision != "SUGGEST" {
		t.Errorf("expected SUGGEST, got %s", decision.Decision)
	}
	if decision.Suggestion != "test-skill: use test pattern" {
		t.Errorf("expected 'test-skill: use test pattern', got %s", decision.Suggestion)
	}
}

func TestEvaluateGuard_HandlesCodeFences(t *testing.T) {
	guardCacheMu.Lock()
	guardCache = make(map[string]guardCacheEntry)
	guardCacheMu.Unlock()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"content": "```json\n{\"decision\":\"PASS\"}\n```",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &guardConfig{
		Model:  "deepseek-v4-flash",
		APIURL: server.URL + "/v1/chat/completions",
		APIKey: "sk-test",
	}

	decision := evaluateGuard(cfg, "rules", "Bash: ls", "Bash")
	if decision.Decision != "PASS" {
		t.Errorf("expected PASS, got %s", decision.Decision)
	}
}

func TestEvaluateGuard_RetriesOnFailure(t *testing.T) {
	guardCacheMu.Lock()
	guardCache = make(map[string]guardCacheEntry)
	guardCacheMu.Unlock()

	var callCount int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		count := callCount
		mu.Unlock()
		if count == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"content": `{"decision":"PASS"}`,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &guardConfig{
		Model:  "deepseek-v4-flash",
		APIURL: server.URL + "/v1/chat/completions",
		APIKey: "sk-test",
	}

	decision := evaluateGuard(cfg, "rules", "Bash: retry-test", "Bash")
	if decision.Decision != "PASS" {
		t.Errorf("expected PASS after retry, got %s", decision.Decision)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls (1 fail + 1 success), got %d", callCount)
	}
}

func TestEvaluateGuard_CacheHit(t *testing.T) {
	guardCacheMu.Lock()
	guardCache = make(map[string]guardCacheEntry)
	guardCacheMu.Unlock()

	var callCount int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"content": `{"decision":"PASS"}`,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &guardConfig{
		Model:  "deepseek-v4-flash",
		APIURL: server.URL + "/v1/chat/completions",
		APIKey: "sk-test",
	}

	// First call
	d1 := evaluateGuard(cfg, "cache rules", "Bash: cache-test", "Bash")
	if d1.Decision != "PASS" {
		t.Errorf("expected PASS, got %s", d1.Decision)
	}

	// Second call with same params — should hit cache
	d2 := evaluateGuard(cfg, "cache rules", "Bash: cache-test", "Bash")
	if d2.Decision != "PASS" {
		t.Errorf("expected PASS from cache, got %s", d2.Decision)
	}

	if callCount != 1 {
		t.Errorf("expected 1 HTTP call (cached), got %d", callCount)
	}
}

func TestDescribeToolCall_Bash(t *testing.T) {
	hook := &HookInput{
		ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"git status"}`),
	}
	desc := describeToolCall(hook)
	if desc != "Bash: git status" {
		t.Errorf("expected 'Bash: git status', got %s", desc)
	}
}

func TestDowngradeUnauthorizedBlock_BashBlockBecomesSuggest(t *testing.T) {
	d := GuardDecision{Decision: "BLOCK", Violations: []string{"Rule 1: auto-commit"}}
	got := downgradeUnauthorizedBlock(d, "Bash")
	if got.Decision != "SUGGEST" {
		t.Errorf("expected SUGGEST, got %s", got.Decision)
	}
	if !strings.Contains(got.Suggestion, "yesmem-remember") {
		t.Errorf("expected mandatory-check skill prefix, got %q", got.Suggestion)
	}
	if !strings.Contains(got.Suggestion, "Rule 1: auto-commit") {
		t.Errorf("expected violations preserved in suggestion, got %q", got.Suggestion)
	}
}

func TestDowngradeUnauthorizedBlock_REPLBlockBecomesSuggest(t *testing.T) {
	d := GuardDecision{Decision: "BLOCK"}
	got := downgradeUnauthorizedBlock(d, "REPL")
	if got.Decision != "SUGGEST" {
		t.Errorf("REPL BLOCK should downgrade, got %s", got.Decision)
	}
	if !strings.Contains(got.Suggestion, "RULES.md") {
		t.Errorf("default suggestion should reference RULES.md, got %q", got.Suggestion)
	}
}

func TestDowngradeUnauthorizedBlock_EditBlockSurvives(t *testing.T) {
	d := GuardDecision{Decision: "BLOCK", Violations: []string{"Rule 2: secret"}}
	got := downgradeUnauthorizedBlock(d, "Edit")
	if got.Decision != "BLOCK" {
		t.Errorf("Edit BLOCK must be honoured, got %s", got.Decision)
	}
}

func TestDowngradeUnauthorizedBlock_WriteBlockSurvives(t *testing.T) {
	d := GuardDecision{Decision: "BLOCK"}
	got := downgradeUnauthorizedBlock(d, "Write")
	if got.Decision != "BLOCK" {
		t.Errorf("Write BLOCK must be honoured, got %s", got.Decision)
	}
}

func TestDowngradeUnauthorizedBlock_NonBlockPassesThrough(t *testing.T) {
	for _, dec := range []string{"PASS", "SUGGEST", ""} {
		d := GuardDecision{Decision: dec, Suggestion: "tdd: x"}
		got := downgradeUnauthorizedBlock(d, "Bash")
		if got.Decision != dec {
			t.Errorf("non-BLOCK %q should pass through, got %s", dec, got.Decision)
		}
	}
}

func TestDescribeToolCall_REPL(t *testing.T) {
	hook := &HookInput{
		ToolName:  "REPL",
		ToolInput: json.RawMessage(`{"code":"sh('git status')"}`),
	}
	desc := describeToolCall(hook)
	if desc != "REPL: sh('git status')" {
		t.Errorf("expected 'REPL: sh(\\'git status\\')', got %s", desc)
	}
}

func TestDescribeToolCall_Edit(t *testing.T) {
	hook := &HookInput{
		ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"/path/to/file.go"}`),
	}
	desc := describeToolCall(hook)
	if desc != "Edit: /path/to/file.go" {
		t.Errorf("expected 'Edit: /path/to/file.go', got %s", desc)
	}
}

func TestDescribeToolCall_Write(t *testing.T) {
	hook := &HookInput{
		ToolName: "Write",
		ToolInput: json.RawMessage(`{"file_path":"/path/to/new.go"}`),
	}
	desc := describeToolCall(hook)
	if desc != "Write: /path/to/new.go" {
		t.Errorf("expected 'Write: /path/to/new.go', got %s", desc)
	}
}

func TestLoadRulesFile_UsesCWD(t *testing.T) {
	td := t.TempDir()
	cwdRule := filepath.Join(td, "RULES.md")
	os.WriteFile(cwdRule, []byte("cwd rules"), 0644)

	rules := loadRulesFile("/nonexistent/RULES.md", td)
	if rules != "cwd rules" {
		t.Errorf("expected 'cwd rules', got %s", rules)
	}
}

func TestLoadRulesFile_Fallback(t *testing.T) {
	td := t.TempDir()
	fallback := filepath.Join(td, "RULES.md")
	os.WriteFile(fallback, []byte("fallback rules"), 0644)

	rules := loadRulesFile(fallback, "")
	if rules != "fallback rules" {
		t.Errorf("expected 'fallback rules', got %s", rules)
	}
}

func TestBuildGuardPrompt_CanBlock(t *testing.T) {
	prompt := buildGuardPrompt("test rule", "Bash: git push", true)
	if !strings.Contains(prompt, "BLOCK") {
		t.Error("expected BLOCK option in prompt for canBlock=true")
	}
	if !strings.Contains(prompt, "SUGGEST") {
		t.Error("expected SUGGEST option in prompt")
	}
	if !strings.Contains(prompt, "PASS") {
		t.Error("expected PASS option in prompt")
	}
}

func TestBuildGuardPrompt_NoBlock(t *testing.T) {
	prompt := buildGuardPrompt("test rule", "Bash: git push", false)
	if strings.Contains(prompt, "BLOCK") {
		t.Error("expected no BLOCK option for canBlock=false")
	}
}

func TestHashStrings_Deterministic(t *testing.T) {
	h1 := hashStrings("a", "b", "c")
	h2 := hashStrings("a", "b", "c")
	if h1 != h2 {
		t.Errorf("expected deterministic hash, got %s vs %s", h1, h2)
	}
	if len(h1) != 16 {
		t.Errorf("expected 16 char hash, got %d: %s", len(h1), h1)
	}
}

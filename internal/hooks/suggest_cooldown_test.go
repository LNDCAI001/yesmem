package hooks

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMaybeSuppressSuggestion_FirstFireRecordsAndEmits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cd.db")
	d := GuardDecision{Decision: "SUGGEST", Suggestion: "yesmem-docs: use search_code_index"}
	now := time.Unix(1700000000, 0)
	suppress := maybeSuppressSuggestion(d, path, 10*time.Minute, now)
	if suppress {
		t.Error("first fire should not be suppressed")
	}
	got := readCooldownTS(t, path, "yesmem-docs")
	if got != now.Unix() {
		t.Errorf("expected cooldown recorded at %d, got %d", now.Unix(), got)
	}
}

func TestMaybeSuppressSuggestion_RepeatWithinTTLSuppresses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cd.db")
	d := GuardDecision{Decision: "SUGGEST", Suggestion: "yesmem-docs: foo"}
	now := time.Unix(1700000000, 0)
	maybeSuppressSuggestion(d, path, 10*time.Minute, now)

	later := now.Add(5 * time.Minute)
	suppress := maybeSuppressSuggestion(d, path, 10*time.Minute, later)
	if !suppress {
		t.Error("repeat within TTL should be suppressed")
	}
	if got := readCooldownTS(t, path, "yesmem-docs"); got != now.Unix() {
		t.Errorf("ts should NOT advance on suppression: got %d want %d", got, now.Unix())
	}
}

func TestMaybeSuppressSuggestion_RepeatAfterTTLEmits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cd.db")
	d := GuardDecision{Decision: "SUGGEST", Suggestion: "yesmem-docs: foo"}
	now := time.Unix(1700000000, 0)
	maybeSuppressSuggestion(d, path, 10*time.Minute, now)

	later := now.Add(11 * time.Minute)
	suppress := maybeSuppressSuggestion(d, path, 10*time.Minute, later)
	if suppress {
		t.Error("repeat after TTL expiry should emit")
	}
	if got := readCooldownTS(t, path, "yesmem-docs"); got != later.Unix() {
		t.Errorf("ts should advance to later fire: got %d want %d", got, later.Unix())
	}
}

func TestMaybeSuppressSuggestion_DifferentSkillsIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cd.db")
	now := time.Unix(1700000000, 0)
	maybeSuppressSuggestion(GuardDecision{Decision: "SUGGEST", Suggestion: "yesmem-docs: a"}, path, 10*time.Minute, now)
	suppress := maybeSuppressSuggestion(GuardDecision{Decision: "SUGGEST", Suggestion: "test-driven-development: b"}, path, 10*time.Minute, now)
	if suppress {
		t.Error("different skill should not be suppressed")
	}
}

func TestMaybeSuppressSuggestion_NonSuggestPassesThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cd.db")
	for _, dec := range []string{"PASS", "BLOCK", ""} {
		d := GuardDecision{Decision: dec, Suggestion: "yesmem-docs: foo"}
		if maybeSuppressSuggestion(d, path, 10*time.Minute, time.Now()) {
			t.Errorf("%q decision should not be cooldown-checked", dec)
		}
	}
}

func TestMaybeSuppressSuggestion_EmptySuggestionPassesThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cd.db")
	d := GuardDecision{Decision: "SUGGEST", Suggestion: ""}
	if maybeSuppressSuggestion(d, path, 10*time.Minute, time.Now()) {
		t.Error("empty suggestion should not be suppressed")
	}
}

func TestMaybeSuppressSuggestion_SkillExtractionTrims(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cd.db")
	now := time.Unix(1700000000, 0)
	maybeSuppressSuggestion(GuardDecision{Decision: "SUGGEST", Suggestion: "  yesmem-docs  : with leading spaces"}, path, 10*time.Minute, now)
	if readCooldownTS(t, path, "yesmem-docs") != now.Unix() {
		t.Error("skill should be normalised (trimmed) before storage")
	}
}

func TestMaybeSuppressSuggestion_UnwritableDBPathFailsOpen(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing-dir", "cd.db")
	d := GuardDecision{Decision: "SUGGEST", Suggestion: "yesmem-docs: foo"}
	if maybeSuppressSuggestion(d, bad, 10*time.Minute, time.Now()) {
		t.Error("DB open failure should not suppress (fail-open, never block guard)")
	}
}

// readCooldownTS opens the cooldown DB and reads back the timestamp for the
// given skill, or zero if absent. Used by tests to assert on stored state.
func readCooldownTS(t *testing.T, dbPath, skill string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var ts int64
	err = db.QueryRow("SELECT last_fired_at FROM guard_cooldown WHERE skill = ?", skill).Scan(&ts)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return ts
}

func TestExtractSkillName(t *testing.T) {
	cases := map[string]string{
		"yesmem-docs: bla":       "yesmem-docs",
		"  tdd  : foo":           "tdd",
		"singleword":             "singleword",
		"": "",
		":only-reason":           "",
	}
	for in, want := range cases {
		if got := extractSkillName(in); got != want {
			t.Errorf("extractSkillName(%q) = %q, want %q", in, got, want)
		}
	}
}

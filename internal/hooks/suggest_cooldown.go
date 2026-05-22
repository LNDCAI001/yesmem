package hooks

import (
	"database/sql"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// extractSkillName pulls "<skill>" from "<skill>: <reason>" suggestion strings.
func extractSkillName(suggestion string) string {
	if idx := strings.Index(suggestion, ":"); idx >= 0 {
		return strings.TrimSpace(suggestion[:idx])
	}
	return strings.TrimSpace(suggestion)
}

// openCooldownDB opens (or creates) the guard cooldown SQLite database.
// Uses WAL + a generous busy timeout so parallel hook-guard processes
// invoked from rapid-fire tool calls don't crash each other.
func openCooldownDB(path string) (*sql.DB, error) {
	uri := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS guard_cooldown (
		skill TEXT PRIMARY KEY,
		last_fired_at INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// maybeSuppressSuggestion returns true if the SUGGEST should be silenced because
// the same skill was suggested within `ttl`. Records the firing in the cooldown
// DB on first emit. Non-SUGGEST decisions and SUGGESTs with no skill name pass
// through unchanged. DB failures pass through (best-effort, never block guard).
func maybeSuppressSuggestion(d GuardDecision, dbPath string, ttl time.Duration, now time.Time) bool {
	if d.Decision != "SUGGEST" || d.Suggestion == "" {
		return false
	}
	skill := extractSkillName(d.Suggestion)
	if skill == "" {
		return false
	}
	db, err := openCooldownDB(dbPath)
	if err != nil {
		return false
	}
	defer db.Close()

	var last int64
	err = db.QueryRow("SELECT last_fired_at FROM guard_cooldown WHERE skill = ?", skill).Scan(&last)
	if err == nil && now.Unix()-last < int64(ttl.Seconds()) {
		return true
	}
	if err != nil && err != sql.ErrNoRows {
		return false
	}

	_, _ = db.Exec("INSERT OR REPLACE INTO guard_cooldown (skill, last_fired_at) VALUES (?, ?)", skill, now.Unix())
	return false
}

// removeLegacyCooldownFile cleans up the deprecated JSON file from the
// pre-DB cooldown implementation. No-op if absent.
func removeLegacyCooldownFile(jsonPath string) {
	_ = os.Remove(jsonPath)
}

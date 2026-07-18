package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/models"
	"github.com/LNDCAI001/yesmem/internal/storage"
)

func TestDeepSearchExcludesCurrentSession(t *testing.T) {
	h, s := mustHandler(t)

	// Insert messages from two different sessions
	now := time.Now().Format("2006-01-02 15:04:05")
	db := s.DB()
	db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES (?, 'assistant', 'text', 'nginx proxy configuration guide', ?, 1)`, "current-session-abc", now)
	db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES (?, 'assistant', 'text', 'nginx proxy troubleshooting tips', ?, 1)`, "other-session-xyz", now)
	// Sync FTS5
	db.Exec(`INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages`)

	// Insert session metadata so project filter doesn't drop them
	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at) VALUES ('current-session-abc', '/test', 1, ?, ?)`, now, now)
	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at) VALUES ('other-session-xyz', '/test', 1, ?, ?)`, now, now)

	// Search WITH exclude_session — should only return other session
	resp := h.Handle(Request{
		Method: "deep_search",
		Params: map[string]any{
			"query":           "nginx proxy",
			"limit":           float64(10),
			"exclude_session": "current-session-abc",
		},
	})
	if resp.Error != "" {
		t.Fatalf("deep_search error: %s", resp.Error)
	}

	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	for _, r := range results {
		if r["session_id"] == "current-session-abc" {
			t.Errorf("deep_search returned current session despite exclude_session")
		}
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result from other session")
	}
	if results[0]["session_id"] != "other-session-xyz" {
		t.Errorf("expected other-session-xyz, got %v", results[0]["session_id"])
	}
}

func TestDeepSearchWithoutExcludeReturnsAll(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	db := s.DB()
	db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES (?, 'assistant', 'text', 'docker compose networking', ?, 1)`, "session-aaa", now)
	db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES (?, 'assistant', 'text', 'docker compose volumes', ?, 1)`, "session-bbb", now)
	db.Exec(`INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages`)

	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at) VALUES ('session-aaa', '/test', 1, ?, ?)`, now, now)
	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at) VALUES ('session-bbb', '/test', 1, ?, ?)`, now, now)

	// Search WITHOUT exclude_session — should return both
	resp := h.Handle(Request{
		Method: "deep_search",
		Params: map[string]any{
			"query": "docker compose",
			"limit": float64(10),
		},
	})
	if resp.Error != "" {
		t.Fatalf("deep_search error: %s", resp.Error)
	}

	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	sessions := map[string]bool{}
	for _, r := range results {
		sessions[r["session_id"].(string)] = true
	}
	if !sessions["session-aaa"] || !sessions["session-bbb"] {
		t.Errorf("expected both sessions without exclude, got %v", sessions)
	}
}

func TestSearchExcludesCurrentSession(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	db := s.DB()
	db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES (?, 'assistant', 'text', 'kubernetes pod scheduling', ?, 1)`, "my-session", now)
	db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES (?, 'assistant', 'text', 'kubernetes pod networking', ?, 1)`, "other-session", now)
	db.Exec(`INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages`)

	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at) VALUES ('my-session', '/test', 1, ?, ?)`, now, now)
	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at) VALUES ('other-session', '/test', 1, ?, ?)`, now, now)

	resp := h.Handle(Request{
		Method: "search",
		Params: map[string]any{
			"query":           "kubernetes pod",
			"limit":           float64(10),
			"exclude_session": "my-session",
		},
	})
	if resp.Error != "" {
		t.Fatalf("search error: %s", resp.Error)
	}

	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	for _, r := range results {
		if r["session_id"] == "my-session" {
			t.Errorf("search returned current session despite exclude_session")
		}
	}
}

func TestSearchIncludesMessageSourceAgent(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	db := s.DB()
	db.Exec(`INSERT INTO messages(session_id, source_agent, role, message_type, content, timestamp, sequence) VALUES (?, ?, 'assistant', 'text', 'agent provenance search hit', ?, 1)`,
		"codex:test-search", models.SourceAgentCodex, now)
	db.Exec(`INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages`)
	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at, source_agent) VALUES ('codex:test-search', '/test', 1, ?, ?, ?)`,
		now, now, models.SourceAgentCodex)

	resp := h.Handle(Request{
		Method: "search",
		Params: map[string]any{
			"query": "agent provenance",
			"limit": float64(10),
		},
	})
	if resp.Error != "" {
		t.Fatalf("search error: %s", resp.Error)
	}

	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0]["source_agent"] != models.SourceAgentCodex {
		t.Fatalf("search result source_agent: got %v, want %q", results[0]["source_agent"], models.SourceAgentCodex)
	}
}

func TestExpandContextQueryDoesNotExcludeSession(t *testing.T) {
	h, s := mustHandler(t)

	now := time.Now().Format("2006-01-02 15:04:05")
	db := s.DB()
	db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES (?, 'assistant', 'text', 'terraform state management', ?, 1)`, "expand-session", now)
	db.Exec(`INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages`)

	s.DB().Exec(`INSERT INTO sessions(id, project, message_count, started_at, indexed_at) VALUES ('expand-session', '/test', 1, ?, ?)`, now, now)

	// expand_context with query should find own session (that's its purpose)
	resp := h.Handle(Request{
		Method: "expand_context",
		Params: map[string]any{
			"query":       "terraform state",
			"_session_id": "expand-session",
		},
	})
	if resp.Error != "" {
		t.Fatalf("expand_context error: %s", resp.Error)
	}

	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	found := false
	for _, r := range results {
		if r["session_id"] == "expand-session" {
			found = true
		}
	}
	if !found {
		t.Error("expand_context should return own session's messages, but didn't")
	}
}

// seedDateMessages inserts three messages dated 2026-04-27/28/29 sharing a unique
// FTS-friendly token so date filtering can be observed via search/deep_search.
func seedDateMessages(t *testing.T, s *storage.Store) {
	t.Helper()
	db := s.DB()
	if _, err := db.Exec(`INSERT INTO sessions(id, project, project_short, message_count, started_at, jsonl_path, indexed_at) VALUES ('date-session', '/test', 'test', 3, ?, '/tmp/date-session.jsonl', ?)`,
		"2026-04-27T10:00:00Z", "2026-04-29T10:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	rows := []struct {
		ts  string
		seq int
	}{
		{"2026-04-27T10:00:00Z", 1},
		{"2026-04-28T19:57:16Z", 2},
		{"2026-04-29T08:30:00Z", 3},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO messages(session_id, role, message_type, content, timestamp, sequence) VALUES ('date-session', 'assistant', 'text', 'needle_dateflag '||?, ?, ?)`, r.ts, r.ts, r.seq); err != nil {
			t.Fatalf("seed message %d: %v", r.seq, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO messages_fts(rowid, content) SELECT id, content FROM messages WHERE id NOT IN (SELECT rowid FROM messages_fts)`); err != nil {
		t.Fatalf("seed fts: %v", err)
	}
}

func TestSearchAcceptsSinceFilter(t *testing.T) {
	h, s := mustHandler(t)
	seedDateMessages(t, s)

	resp := h.Handle(Request{
		Method: "search",
		Params: map[string]any{
			"query": "needle_dateflag",
			"since": "2026-04-28",
			"limit": float64(50),
		},
	})
	if resp.Error != "" {
		t.Fatalf("search error: %s", resp.Error)
	}
	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	for _, r := range results {
		ts, _ := r["timestamp"].(string)
		if ts < "2026-04-28" {
			t.Errorf("since=2026-04-28 returned earlier hit ts=%q", ts)
		}
	}
	if len(results) == 0 {
		t.Fatal("expected at least one hit on/after 2026-04-28")
	}
}

func TestSearchAcceptsBeforeFilter(t *testing.T) {
	h, s := mustHandler(t)
	seedDateMessages(t, s)

	resp := h.Handle(Request{
		Method: "search",
		Params: map[string]any{
			"query":  "needle_dateflag",
			"before": "2026-04-29",
			"limit":  float64(50),
		},
	})
	if resp.Error != "" {
		t.Fatalf("search error: %s", resp.Error)
	}
	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	for _, r := range results {
		ts, _ := r["timestamp"].(string)
		if ts >= "2026-04-29" {
			t.Errorf("before=2026-04-29 returned later hit ts=%q", ts)
		}
	}
	if len(results) == 0 {
		t.Fatal("expected at least one hit before 2026-04-29")
	}
}

func TestDeepSearchAcceptsDateBounds(t *testing.T) {
	h, s := mustHandler(t)
	seedDateMessages(t, s)

	resp := h.Handle(Request{
		Method: "deep_search",
		Params: map[string]any{
			"query":  "needle_dateflag",
			"since":  "2026-04-28",
			"before": "2026-04-29",
			"limit":  float64(50),
		},
	})
	if resp.Error != "" {
		t.Fatalf("deep_search error: %s", resp.Error)
	}
	var results []map[string]any
	b, _ := json.Marshal(resp.Result)
	json.Unmarshal(b, &results)

	if len(results) != 1 {
		t.Fatalf("expected exactly one hit on 2026-04-28, got %d", len(results))
	}
	for _, r := range results {
		ts, _ := r["timestamp"].(string)
		if ts < "2026-04-28" || ts >= "2026-04-29" {
			t.Errorf("date window violated: ts=%q", ts)
		}
	}
}

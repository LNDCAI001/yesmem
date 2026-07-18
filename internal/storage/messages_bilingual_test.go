package storage

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/models"
)

// seedBilingualMessages seeds three messages:
//  1. matches BOTH DE and EN queries (proxy cache + fehler/error)
//  2. DE-only content (datenbank verbindung fehlgeschlagen)
//  3. EN-only content (database connection failed)
func seedBilingualMessages(t *testing.T, s *Store) {
	t.Helper()
	sess := &models.Session{
		ID:        "test-bilingual",
		Project:   "/proj",
		StartedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		IndexedAt: time.Now().UTC(),
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	mk := func(seq int, body string) models.Message {
		return models.Message{
			SessionID:   sess.ID,
			Role:        "user",
			MessageType: "text",
			Content:     body,
			Timestamp:   time.Date(2026, 5, 1, 12, seq, 0, 0, time.UTC),
			Sequence:    seq,
		}
	}
	msgs := []models.Message{
		mk(1, "proxy cache fehler kaputt proxy cache error broken"),
		mk(2, "datenbank verbindung fehlgeschlagen"),
		mk(3, "database connection failed"),
	}
	if err := s.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}
}

// Golden path: empty queryEn must behave identically to SearchMessagesCtx.
func TestSearchMessagesBilingualCtx_GoldenPath(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	seedBilingualMessages(t, s)

	standard, err := s.SearchMessagesCtx("proxy cache", "", "", 100)
	if err != nil {
		t.Fatalf("standard: %v", err)
	}
	bilingual, err := s.SearchMessagesBilingualCtx("proxy cache", "", "", "", 100)
	if err != nil {
		t.Fatalf("bilingual: %v", err)
	}
	if len(standard) != len(bilingual) {
		t.Fatalf("golden path broken: standard=%d hits, bilingual=%d", len(standard), len(bilingual))
	}
	for i := range standard {
		if standard[i].ID != bilingual[i].ID {
			t.Fatalf("golden path broken at pos %d: standard=%d, bilingual=%d", i, standard[i].ID, bilingual[i].ID)
		}
	}
}

// Fusion must surface hits from both language lanes.
func TestSearchMessagesBilingualCtx_FusesBothLanguages(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	seedBilingualMessages(t, s)

	hits, err := s.SearchMessagesBilingualCtx("datenbank verbindung", "database connection", "", "", 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected fusion to find DE-only and EN-only messages, got %d hits", len(hits))
	}
	contents, err := s.GetMessageContents(hitsToIDs(hits))
	if err != nil {
		t.Fatalf("load contents: %v", err)
	}
	var sawDE, sawEN bool
	for _, c := range contents {
		if strings.Contains(c, "datenbank") {
			sawDE = true
		}
		if strings.Contains(c, "database") {
			sawEN = true
		}
	}
	if !sawDE || !sawEN {
		t.Fatalf("fusion did not cover both languages: sawDE=%v sawEN=%v", sawDE, sawEN)
	}
}

// A message matching both queries must not appear twice in the fused output.
func TestSearchMessagesBilingualCtx_Dedup(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	seedBilingualMessages(t, s)

	hits, err := s.SearchMessagesBilingualCtx("proxy cache fehler", "proxy cache error", "", "", 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	seen := make(map[int64]int)
	for _, h := range hits {
		seen[h.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("message id %d appeared %d times in fused output", id, n)
		}
	}
}

// Order must be stable across repeated calls — guards against sort.Slice
// random tie-breaking, which would flake downstream consumers.
func TestSearchMessagesBilingualCtx_DeterministicOrder(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	sess := &models.Session{
		ID:        "test-bilingual-det",
		Project:   "/proj",
		StartedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		IndexedAt: time.Now().UTC(),
	}
	if err := s.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	var msgs []models.Message
	for i := 1; i <= 10; i++ {
		msgs = append(msgs, models.Message{
			SessionID:   sess.ID,
			Role:        "user",
			MessageType: "text",
			Content:     fmt.Sprintf("marker unique%d", i),
			Timestamp:   time.Date(2026, 5, 1, 12, i, 0, 0, time.UTC),
			Sequence:    i,
		})
	}
	if err := s.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	var firstOrder []int64
	for trial := 0; trial < 30; trial++ {
		hits, err := s.SearchMessagesBilingualCtx("marker", "marker", "", "", 100)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		if len(hits) != 10 {
			t.Fatalf("trial %d: expected 10 hits, got %d", trial, len(hits))
		}
		order := make([]int64, len(hits))
		for i, h := range hits {
			order[i] = h.ID
		}
		if firstOrder == nil {
			firstOrder = order
			continue
		}
		for i := range firstOrder {
			if firstOrder[i] != order[i] {
				t.Fatalf("trial %d: nondeterministic order at pos %d: was %d, now %d (sort.Slice needs stable tiebreak)",
					trial, i, firstOrder[i], order[i])
			}
		}
	}
}

func hitsToIDs(hits []MessageSearchResult) []int64 {
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	return ids
}

// --- Deep bilingual ---

func TestSearchMessagesDeepBilingualCtx_GoldenPath(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	seedBilingualMessages(t, s)

	standard, err := s.SearchMessagesDeepCtx("proxy cache", false, false, "", "", 100)
	if err != nil {
		t.Fatalf("standard: %v", err)
	}
	bilingual, err := s.SearchMessagesDeepBilingualCtx("proxy cache", "", false, false, "", "", 100)
	if err != nil {
		t.Fatalf("bilingual: %v", err)
	}
	if len(standard) != len(bilingual) {
		t.Fatalf("deep golden path broken: standard=%d, bilingual=%d", len(standard), len(bilingual))
	}
	for i := range standard {
		if standard[i].ID != bilingual[i].ID {
			t.Fatalf("deep golden path broken at pos %d: standard=%d, bilingual=%d", i, standard[i].ID, bilingual[i].ID)
		}
	}
}

func TestSearchMessagesDeepBilingualCtx_Fuses(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	seedBilingualMessages(t, s)

	hits, err := s.SearchMessagesDeepBilingualCtx("datenbank verbindung", "database connection", false, false, "", "", 100)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected deep fusion to find both languages, got %d hits", len(hits))
	}
}

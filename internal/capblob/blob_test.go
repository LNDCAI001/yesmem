package capblob

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/storage"
)

type mockRow struct {
	id   int64
	data map[string]any
}

type mockStore struct {
	tables map[string]map[string]bool
	rows   map[string][]mockRow
	nextID int64
}

func newMockStore() *mockStore {
	return &mockStore{
		tables: map[string]map[string]bool{},
		rows:   map[string][]mockRow{},
		nextID: 1,
	}
}

func (m *mockStore) tableKey(cap, tbl string) string {
	return cap + "::" + tbl
}

func (m *mockStore) CapStoreCreateTable(capName, tableName string, columns []storage.ColumnDef) error {
	if m.tables[capName] == nil {
		m.tables[capName] = map[string]bool{}
	}
	m.tables[capName][tableName] = true
	return nil
}

func (m *mockStore) CapStoreUpsert(capName, tableName string, data map[string]any) (int64, error) {
	if !m.tables[capName][tableName] {
		return 0, fmt.Errorf("table not created")
	}
	k := m.tableKey(capName, tableName)
	id := m.nextID
	m.nextID++
	row := mockRow{id: id, data: map[string]any{"id": id}}
	for kk, vv := range data {
		row.data[kk] = vv
	}
	m.rows[k] = append(m.rows[k], row)
	return id, nil
}

func (m *mockStore) CapStoreQuery(capName, tableName, where string, args []any, limit int) ([]map[string]any, error) {
	k := m.tableKey(capName, tableName)
	var out []map[string]any
	for _, row := range m.rows[k] {
		if where == "key=?" && len(args) == 1 {
			if row.data["key"] != args[0] {
				continue
			}
		}
		copyRow := map[string]any{}
		for kk, vv := range row.data {
			copyRow[kk] = vv
		}
		if idx, ok := copyRow["chunk_idx"].(int); ok {
			copyRow["chunk_idx"] = int64(idx)
		}
		out = append(out, copyRow)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *mockStore) CapStoreDelete(capName, tableName, where string, args []any) (int64, error) {
	k := m.tableKey(capName, tableName)
	var kept []mockRow
	var deleted int64
	for _, row := range m.rows[k] {
		if where == "key=?" && len(args) == 1 && row.data["key"] == args[0] {
			deleted++
			continue
		}
		kept = append(kept, row)
	}
	m.rows[k] = kept
	return deleted, nil
}

func TestPutGet_SingleChunk(t *testing.T) {
	s := newMockStore()
	want := "hello world"
	if err := Put(s, "testcap", "k1", strings.NewReader(want), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var buf bytes.Buffer
	if err := Get(s, "testcap", "k1", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != want {
		t.Fatalf("mismatch: got %q want %q", buf.String(), want)
	}
}

func TestPutGet_MultiChunk(t *testing.T) {
	s := newMockStore()
	payload := strings.Repeat("abcdefgh", 40000)
	if len(payload) < DefaultChunkSize*3 {
		t.Fatalf("test payload too small: %d", len(payload))
	}
	if err := Put(s, "testcap", "big", strings.NewReader(payload), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rows, _ := s.CapStoreQuery("testcap", TableName, "key=?", []any{"big"}, QueryLimit)
	if len(rows) < 3 {
		t.Fatalf("expected >=3 chunks, got %d", len(rows))
	}
	var buf bytes.Buffer
	if err := Get(s, "testcap", "big", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != payload {
		t.Fatalf("size mismatch: got %d want %d", buf.Len(), len(payload))
	}
}

func TestPut_OverwriteShrinks(t *testing.T) {
	s := newMockStore()
	big := strings.Repeat("X", DefaultChunkSize*3+100)
	if err := Put(s, "c", "k", strings.NewReader(big), DefaultChunkSize); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	small := "tiny"
	if err := Put(s, "c", "k", strings.NewReader(small), DefaultChunkSize); err != nil {
		t.Fatalf("Put small: %v", err)
	}
	rows, _ := s.CapStoreQuery("c", TableName, "key=?", []any{"k"}, QueryLimit)
	if len(rows) != 1 {
		t.Fatalf("expected 1 chunk after shrink, got %d", len(rows))
	}
	var buf bytes.Buffer
	if err := Get(s, "c", "k", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != small {
		t.Fatalf("content: got %q want %q", buf.String(), small)
	}
}

func TestGet_UnknownKey(t *testing.T) {
	s := newMockStore()
	if err := s.CapStoreCreateTable("c", TableName, schema()); err != nil {
		t.Fatalf("createTable: %v", err)
	}
	var buf bytes.Buffer
	if err := Get(s, "c", "nope", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty, got %q", buf.String())
	}
}

func TestPut_EmptyInput(t *testing.T) {
	s := newMockStore()
	if err := Put(s, "c", "k", strings.NewReader(""), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rows, _ := s.CapStoreQuery("c", TableName, "key=?", []any{"k"}, QueryLimit)
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows for empty input, got %d", len(rows))
	}
}

func TestPutGet_NonAsciiBytes(t *testing.T) {
	s := newMockStore()
	payload := "Ümlaut-Test: €ñ日本 " + strings.Repeat("αβγ", 20000)
	if err := Put(s, "c", "utf", strings.NewReader(payload), DefaultChunkSize); err != nil {
		t.Fatalf("Put: %v", err)
	}
	var buf bytes.Buffer
	if err := Get(s, "c", "utf", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != payload {
		t.Fatalf("utf mismatch")
	}
}

func TestGet_SortsByChunkIdx(t *testing.T) {
	s := newMockStore()
	if err := s.CapStoreCreateTable("c", TableName, schema()); err != nil {
		t.Fatalf("createTable: %v", err)
	}
	s.CapStoreUpsert("c", TableName, map[string]any{"key": "k", "chunk_idx": 2, "data": "C"})
	s.CapStoreUpsert("c", TableName, map[string]any{"key": "k", "chunk_idx": 0, "data": "A"})
	s.CapStoreUpsert("c", TableName, map[string]any{"key": "k", "chunk_idx": 1, "data": "B"})
	var buf bytes.Buffer
	if err := Get(s, "c", "k", &buf); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if buf.String() != "ABC" {
		t.Fatalf("sort order wrong: %q", buf.String())
	}
}

func TestPut_CreateTableIdempotent(t *testing.T) {
	s := newMockStore()
	for i := 0; i < 3; i++ {
		if err := Put(s, "c", fmt.Sprintf("k%d", i), strings.NewReader("x"), DefaultChunkSize); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	rows, _ := s.CapStoreQuery("c", TableName, "", nil, QueryLimit)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
}

// Pins DefaultChunkSize small enough that a single stored chunk fits in the
// MCP JSON-RPC response limit (~25 KB/row). Larger chunks make blobs
// unreadable via mcp__yesmem__cap_store: the row payload trips the MCP
// client's result-size cap and returns an error string instead of JSON.
func TestPut_ChunksStayBelowMCPResponseCap(t *testing.T) {
	const mcpSafeMax = 25000
	s := newMockStore()
	payload := strings.Repeat("x", 200*1024)
	if err := Put(s, "c", "k", strings.NewReader(payload), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rows, _ := s.CapStoreQuery("c", TableName, "key=?", []any{"k"}, QueryLimit)
	if len(rows) == 0 {
		t.Fatalf("expected chunks, got 0")
	}
	for _, row := range rows {
		data, ok := row["data"].(string)
		if !ok {
			t.Fatalf("row data not string: %T", row["data"])
		}
		if len(data) > mcpSafeMax {
			t.Errorf("chunk_idx=%v exceeds MCP-safe size: %d > %d",
				row["chunk_idx"], len(data), mcpSafeMax)
		}
	}
}

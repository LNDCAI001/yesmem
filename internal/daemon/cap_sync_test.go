package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LNDCAI001/yesmem/internal/capfile"
)

type stubDDLLooker struct {
	ddl string
}

func (s *stubDDLLooker) GetCapTableDDL(capName string) (string, error) {
	return s.ddl, nil
}

func TestCapFileToParams(t *testing.T) {
	cf := capfile.CapFile{
		Name:        "test_tool",
		Description: "A test tool",
		Tags:        []string{"test", "example"},
		Tested:      true,
		AutoActive:  false,
		Scripts: []capfile.Script{
			{
				Name:    "test_tool",
				Kind:    "tool",
				Runtime: "repl",
				Lang:    "javascript",
				Body:    "async () => 42",
				Schema:  `{"type":"object"}`,
			},
		},
	}

	params := CapFileToParams(cf)

	if params["name"] != "test_tool" {
		t.Errorf("name = %v, want test_tool", params["name"])
	}
	if params["description"] != "A test tool" {
		t.Errorf("description mismatch")
	}
	scriptsJSON, ok := params["scripts"].(string)
	if !ok || scriptsJSON == "" {
		t.Fatalf("scripts param missing or wrong type: %T", params["scripts"])
	}
	var scripts []map[string]any
	if err := json.Unmarshal([]byte(scriptsJSON), &scripts); err != nil {
		t.Fatalf("scripts JSON invalid: %v", err)
	}
	if len(scripts) != 1 {
		t.Fatalf("expected 1 script in JSON, got %d", len(scripts))
	}
	if scripts[0]["body"] != "async () => 42" {
		t.Errorf("script body = %v, want async () => 42", scripts[0]["body"])
	}
	if scripts[0]["runtime"] != "repl" {
		t.Errorf("script runtime = %v, want repl", scripts[0]["runtime"])
	}
}

func TestCapFileToParams_BashRuntime(t *testing.T) {
	cf := capfile.CapFile{
		Name:        "bash_tool",
		Description: "Bash tool",
		Scripts: []capfile.Script{
			{
				Name:    "bash_tool",
				Kind:    "tool",
				Runtime: "bash",
				Lang:    "bash",
				Body:    "echo hi",
				Schema:  `{"type":"object"}`,
			},
		},
	}

	params := CapFileToParams(cf)

	scriptsJSON, _ := params["scripts"].(string)
	var scripts []map[string]any
	if err := json.Unmarshal([]byte(scriptsJSON), &scripts); err != nil {
		t.Fatalf("scripts JSON invalid: %v", err)
	}
	if scripts[0]["runtime"] != "bash" {
		t.Errorf("runtime = %v, want bash", scripts[0]["runtime"])
	}
	if scripts[0]["body"] != "echo hi" {
		t.Errorf("body = %v, want 'echo hi'", scripts[0]["body"])
	}
}

func TestWriteCapToDisk_CreatesFile(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "test_tool",
		Description: "A test tool",
		Scripts: []ScriptMeta{
			{Name: "test_tool", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: `{"type":"object"}`},
		},
		Tags: []string{"test"},
	}

	if err := WriteCapToDisk(meta, "", dir, nil); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	want := filepath.Join(dir, "test_tool", "CAP.md")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("CAP.md not written: %v", err)
	}
	if !strings.Contains(string(data), "test_tool") {
		t.Errorf("CAP.md missing cap name")
	}
	if !strings.Contains(string(data), "echo hi") {
		t.Errorf("CAP.md missing script body")
	}
}

func TestWriteCapToDisk_HydratesDatabaseSQLFromStore(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "test_tool",
		Description: "Has DB",
		Scripts: []ScriptMeta{
			{Name: "test_tool", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: `{"type":"object"}`},
		},
	}

	looker := &stubDDLLooker{ddl: "CREATE TABLE cap_test_tool__items (id TEXT PRIMARY KEY);"}

	if err := WriteCapToDisk(meta, "", dir, looker); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "test_tool", "CAP.md"))
	if !strings.Contains(string(data), "CREATE TABLE cap_test_tool__items") {
		t.Errorf("Database section missing DDL — content:\n%s", string(data))
	}
}

func TestWriteCapToDisk_NilLookerSkipsHydration(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "test_tool",
		Description: "No DB",
		Scripts: []ScriptMeta{
			{Name: "test_tool", Kind: "tool", Runtime: "bash", Body: "echo hi", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "test_tool", "CAP.md"))
	// Without DDL, the Database section must be omitted entirely (spec: optional).
	if strings.Contains(string(data), "## Database") {
		t.Errorf("Database section should be omitted when DDL empty:\n%s", string(data))
	}
}

func TestCapMetaToCapFile(t *testing.T) {
	meta := CapMeta{
		Name:        "myrepl",
		Description: "REPL tool",
		Scripts: []ScriptMeta{
			{Name: "myrepl", Kind: "tool", Runtime: "repl", Body: "async () => 42", Schema: `{"type":"object"}`},
		},
		Tags: []string{"a"},
	}
	cf := CapMetaToCapFile(meta, "myproject")

	if cf.Name != "myrepl" {
		t.Errorf("Name = %q, want myrepl", cf.Name)
	}
	if cf.Scope != "project" {
		t.Errorf("Scope = %q, want project", cf.Scope)
	}
	if len(cf.Scripts) != 1 {
		t.Fatalf("Scripts: want 1, got %d", len(cf.Scripts))
	}
	sc := cf.Scripts[0]
	if sc.Runtime != "repl" {
		t.Errorf("Script.Runtime = %q, want repl", sc.Runtime)
	}
	if sc.Body != "async () => 42" {
		t.Errorf("Script.Body = %q, want async () => 42", sc.Body)
	}
	if sc.Lang != "javascript" {
		t.Errorf("Script.Lang = %q, want javascript", sc.Lang)
	}
}

func TestCapMetaToCapFile_NoProject(t *testing.T) {
	meta := CapMeta{
		Name:        "global_tool",
		Description: "Global",
		Scripts: []ScriptMeta{
			{Name: "global_tool", Kind: "tool", Runtime: "bash", Body: "echo global", Schema: `{"type":"object"}`},
		},
	}
	cf := CapMetaToCapFile(meta, "")

	if cf.Scope != "user" {
		t.Errorf("Scope = %q, want user", cf.Scope)
	}
	if len(cf.Scripts) != 1 {
		t.Fatalf("Scripts: want 1, got %d", len(cf.Scripts))
	}
	if cf.Scripts[0].Runtime != "bash" {
		t.Errorf("Script.Runtime = %q, want bash", cf.Scripts[0].Runtime)
	}
}

const v1Cap = "---\nname: my_cap\ndescription: %s\n---\n\n## Purpose\n\nTest.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho %s\n```\n"

func TestCapsDirWatcher_DetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	w := NewCapsDirWatcher()

	if changed := w.ScanChanged(dir); len(changed) != 0 {
		t.Fatalf("empty dir: want 0 changed, got %d", len(changed))
	}

	capDir := filepath.Join(dir, "my_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nname: my_cap\ndescription: test\n---\n\n## Purpose\n\nTest.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho hi\n```\n")
	if err := os.WriteFile(filepath.Join(capDir, "CAP.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	changed := w.ScanChanged(dir)
	if len(changed) != 1 {
		t.Fatalf("new file: want 1 changed, got %d", len(changed))
	}
	if changed[0].Name != "my_cap" {
		t.Errorf("name: want my_cap, got %q", changed[0].Name)
	}

	if changed = w.ScanChanged(dir); len(changed) != 0 {
		t.Errorf("no change: want 0, got %d", len(changed))
	}
}

func TestCapsDirWatcher_DetectsModifiedFile(t *testing.T) {
	dir := t.TempDir()
	w := NewCapsDirWatcher()

	capDir := filepath.Join(dir, "my_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(capDir, "CAP.md")
	body1 := []byte("---\nname: my_cap\ndescription: v1\n---\n\n## Purpose\n\nV1.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho v1\n```\n")
	if err := os.WriteFile(capPath, body1, 0o644); err != nil {
		t.Fatal(err)
	}
	w.ScanChanged(dir)

	body2 := []byte("---\nname: my_cap\ndescription: v2\n---\n\n## Purpose\n\nV2.\n\n## Scripts\n\n### my_cap\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\necho v2\n```\n")
	if err := os.WriteFile(capPath, body2, 0o644); err != nil {
		t.Fatal(err)
	}

	changed := w.ScanChanged(dir)
	if len(changed) != 1 {
		t.Fatalf("modified: want 1 changed, got %d", len(changed))
	}
	if changed[0].Description != "v2" {
		t.Errorf("description: want v2, got %q", changed[0].Description)
	}
}

func TestCapsDirWatcher_IgnoresParseErrors(t *testing.T) {
	dir := t.TempDir()
	w := NewCapsDirWatcher()

	capDir := filepath.Join(dir, "bad_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "CAP.md"), []byte("not valid yaml frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}

	if changed := w.ScanChanged(dir); len(changed) != 0 {
		t.Errorf("parse error: want 0 changed, got %d", len(changed))
	}
}

func writeDiskCap(t *testing.T, dir, name string, version int, body string) string {
	t.Helper()
	capDir := filepath.Join(dir, name)
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: disk content\nversion: %d\n---\n\n## Scripts\n\n### %s\nkind: tool\nschema: {\"type\":\"object\"}\n\n```bash\n%s\n```\n", name, version, name, body)
	capPath := filepath.Join(capDir, "CAP.md")
	if err := os.WriteFile(capPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return capPath
}

func readDiskCap(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read disk cap: %v", err)
	}
	return string(data)
}

func TestWriteCapToDisk_SkipsWhenDiskVersionEqual(t *testing.T) {
	dir := t.TempDir()
	capPath := writeDiskCap(t, dir, "my_cap", 5, "echo disk_v5")

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo disk_v5") {
		t.Errorf("disk content was overwritten despite equal version:\n%s", content)
	}
	if strings.Contains(content, "echo db_v5") {
		t.Errorf("disk content was replaced with DB content:\n%s", content)
	}
}

func TestWriteCapToDisk_SkipsWhenDiskVersionHigher(t *testing.T) {
	dir := t.TempDir()
	capPath := writeDiskCap(t, dir, "my_cap", 7, "echo disk_v7")

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo disk_v7") {
		t.Errorf("disk content was overwritten despite higher version:\n%s", content)
	}
}

func TestWriteCapToDisk_OverwritesWhenDiskVersionLower(t *testing.T) {
	dir := t.TempDir()
	capPath := writeDiskCap(t, dir, "my_cap", 3, "echo disk_v3")

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo db_v5") {
		t.Errorf("disk content was NOT overwritten despite lower version:\n%s", content)
	}
}

func TestWriteCapToDisk_OverwritesWhenDiskMissing(t *testing.T) {
	dir := t.TempDir()

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	capPath := filepath.Join(dir, "my_cap", "CAP.md")
	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatalf("CAP.md not written: %v", err)
	}
	if !strings.Contains(string(data), "echo db_v5") {
		t.Errorf("CAP.md missing expected content:\n%s", string(data))
	}
}

func TestWriteCapToDisk_OverwritesWhenDiskCorrupt(t *testing.T) {
	dir := t.TempDir()
	capDir := filepath.Join(dir, "my_cap")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(capDir, "CAP.md")
	if err := os.WriteFile(capPath, []byte("not valid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta := CapMeta{
		Name:        "my_cap",
		Description: "db content",
		Version:     5,
		Scripts: []ScriptMeta{
			{Name: "my_cap", Kind: "tool", Runtime: "bash", Body: "echo db_v5", Schema: `{"type":"object"}`},
		},
	}

	if err := WriteCapToDisk(meta, "", dir, nil); err != nil {
		t.Fatalf("WriteCapToDisk: %v", err)
	}

	content := readDiskCap(t, capPath)
	if !strings.Contains(content, "echo db_v5") {
		t.Errorf("corrupt disk content was NOT overwritten:\n%s", content)
	}
}

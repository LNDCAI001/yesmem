package daemon

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// captureLog redirects the standard logger into a buffer for the duration
// of the test and restores it afterwards.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestPollOpencodeSessionID_LogsWhenDBMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := captureLog(t)

	if got := pollOpencodeSessionID("/some/workdir", 10*time.Millisecond); got != "" {
		t.Fatalf("expected empty session id, got %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "opencode.db") || !strings.Contains(out, "not found") {
		t.Errorf("expected log about missing opencode.db, got: %q", out)
	}
}

func TestPollOpencodeSessionID_LogsTimeoutWithCandidates(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dbDir := home + "/.local/share/opencode"
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `CREATE TABLE session (id TEXT, directory TEXT, time_created INTEGER);
INSERT INTO session VALUES ('ses_other', '/other/project', 1);`
	cmd := exec.Command("sqlite3", dbDir+"/opencode.db", seed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed db: %v (%s)", err, out)
	}
	buf := captureLog(t)

	if got := pollOpencodeSessionID("/wanted/project", 10*time.Millisecond); got != "" {
		t.Fatalf("expected empty session id, got %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "/wanted/project") {
		t.Errorf("expected timeout log to name the workDir, got: %q", out)
	}
	if !strings.Contains(out, "/other/project") {
		t.Errorf("expected timeout log to list recent session directories, got: %q", out)
	}
}

func TestPollCodexSessionID_LogsTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	buf := captureLog(t)

	if got := pollCodexSessionID("/some/workdir", 10*time.Millisecond); got != "" {
		t.Fatalf("expected empty session id, got %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "/some/workdir") {
		t.Errorf("expected timeout log to name the workDir, got: %q", out)
	}
}

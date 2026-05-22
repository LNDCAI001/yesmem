package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrateGuardState_RemovesLegacyJSON(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "yesmem")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "guard_suggest_cooldown.json")
	if err := os.WriteFile(legacy, []byte(`{"yesmem-docs":1700000000}`), 0644); err != nil {
		t.Fatal(err)
	}
	removed, err := MigrateGuardState(home)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if removed != legacy {
		t.Errorf("expected removed path %q, got %q", legacy, removed)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy file should be gone, stat err: %v", err)
	}
}

func TestMigrateGuardState_AbsentFileIsNoop(t *testing.T) {
	home := t.TempDir()
	removed, err := MigrateGuardState(home)
	if err != nil {
		t.Fatalf("migrate on absent: %v", err)
	}
	if removed != "" {
		t.Errorf("expected empty removed path when nothing to clean, got %q", removed)
	}
}

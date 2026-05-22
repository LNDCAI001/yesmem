package setup

import (
	"os"
	"path/filepath"
)

// MigrateGuardState performs one-shot migrations for hook-guard runtime state.
// Currently: removes the deprecated guard_suggest_cooldown.json from the JSON-file
// cooldown implementation. The DB-backed replacement (guard_state.db) is
// auto-created on first hook-guard invocation, so no positive setup step is
// required here — only legacy cleanup.
//
// Returns the path that was removed (empty if nothing to remove), and any
// error encountered. Missing files are not errors.
func MigrateGuardState(home string) (string, error) {
	legacy := filepath.Join(home, ".claude", "yesmem", "guard_suggest_cooldown.json")
	if _, err := os.Stat(legacy); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if err := os.Remove(legacy); err != nil {
		return "", err
	}
	return legacy, nil
}

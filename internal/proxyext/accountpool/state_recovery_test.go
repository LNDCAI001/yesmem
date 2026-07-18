package accountpool

import (
	"testing"
	"time"
)

// TestHardFailSelfHeals verifies the pool re-probes a hard-failed account once
// the recovery window elapses, instead of benching it until a daemon restart.
// This is the fix for the 48h pool lockup: an OAuth token that expired during a
// long WSL-only session hard-fails both accounts, and without recovery the pool
// stays dead even after the token is refreshed on the host.
func TestHardFailSelfHeals(t *testing.T) {
	store := NewStateStore([]AccountRef{{Name: "a", CredentialDir: "~/x"}})
	for i := 0; i < 3; i++ {
		store.RecordAuthError("a", 3)
	}
	if store.IsAvailable("a") {
		t.Fatal("account should be hard-failed after 3 consecutive auth errors")
	}
	// Age the last auth error past the recovery window.
	store.mu.Lock()
	store.states["a"].LastAuthErrorAt = time.Now().Add(-hardFailRecovery - time.Minute)
	store.mu.Unlock()
	if !store.IsAvailable("a") {
		t.Fatal("hard-failed account should re-probe (become available) after recovery window")
	}
	st, _ := store.Get("a")
	if st.Status != StatusAvailable {
		t.Fatalf("status should be Available after recovery probe, got %v", st.Status)
	}
}

// TestHardFailStaysWithinWindow verifies a freshly hard-failed account is not
// prematurely re-probed inside the recovery window.
func TestHardFailStaysWithinWindow(t *testing.T) {
	store := NewStateStore([]AccountRef{{Name: "a", CredentialDir: "~/x"}})
	for i := 0; i < 3; i++ {
		store.RecordAuthError("a", 3)
	}
	if store.IsAvailable("a") {
		t.Fatal("hard-failed account should stay unavailable within the recovery window")
	}
}

// TestSetEnabledTogglesSelection verifies runtime enable/disable.
func TestSetEnabledTogglesSelection(t *testing.T) {
	store := NewStateStore([]AccountRef{{Name: "a", CredentialDir: "~/x"}})
	if !store.IsAvailable("a") {
		t.Fatal("should start available")
	}
	if !store.SetEnabled("a", false) {
		t.Fatal("SetEnabled(false) should succeed for a known account")
	}
	if store.IsAvailable("a") {
		t.Fatal("disabled account must not be selectable")
	}
	if !store.SetEnabled("a", true) {
		t.Fatal("re-enable should succeed")
	}
	if !store.IsAvailable("a") {
		t.Fatal("re-enabled account should be selectable again")
	}
	if store.SetEnabled("unknown", true) {
		t.Fatal("SetEnabled on unknown account should return false")
	}
}

// TestWithinActiveWindow covers no-schedule, normal window, out-of-window,
// wrap-around midnight, and fail-open on a bad timezone.
func TestWithinActiveWindow(t *testing.T) {
	noon := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	night := time.Date(2026, 7, 18, 23, 0, 0, 0, time.UTC)

	if !withinActiveWindow(AccountRef{}, noon) {
		t.Fatal("empty tz = always active")
	}
	if !withinActiveWindow(AccountRef{ActiveTZ: "UTC", ActiveStartHour: 9, ActiveEndHour: 17}, noon) {
		t.Fatal("12:00 within 09-17 should be active")
	}
	if withinActiveWindow(AccountRef{ActiveTZ: "UTC", ActiveStartHour: 13, ActiveEndHour: 17}, noon) {
		t.Fatal("12:00 is not within 13-17")
	}
	if withinActiveWindow(AccountRef{ActiveTZ: "UTC", ActiveStartHour: 21, ActiveEndHour: 6}, noon) {
		t.Fatal("12:00 is not within wrap-around 21-6")
	}
	if !withinActiveWindow(AccountRef{ActiveTZ: "UTC", ActiveStartHour: 21, ActiveEndHour: 6}, night) {
		t.Fatal("23:00 within wrap-around 21-6 should be active")
	}
	if !withinActiveWindow(AccountRef{ActiveTZ: "Not/AZone", ActiveStartHour: 1, ActiveEndHour: 2}, noon) {
		t.Fatal("unparseable tz must fail open (always active)")
	}
}

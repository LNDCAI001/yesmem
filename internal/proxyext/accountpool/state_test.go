package accountpool_test

import (
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/proxyext/accountpool"
)

func makeAccountsAlt() []accountpool.AccountRef {
	return []accountpool.AccountRef{
		{Name: "acct1", CredentialDir: "~/.claude-acct1"},
		{Name: "acct2", CredentialDir: "~/.claude-acct2"},
	}
}

func TestStateStoreAvailableAfterSuccess(t *testing.T) {
	store := accountpool.NewStateStore(makeAccountsAlt())
	store.RecordSuccess("acct1")
	if !store.IsAvailable("acct1") {
		t.Fatal("should be available after success")
	}
}

func TestStateStoreCooldownPreventsSelection(t *testing.T) {
	store := accountpool.NewStateStore(makeAccountsAlt())
	store.RecordQuotaHit("acct1", 10*time.Minute)
	if store.IsAvailable("acct1") {
		t.Fatal("should NOT be available during cooldown")
	}
}

func TestStateStoreCooldownExpires(t *testing.T) {
	store := accountpool.NewStateStore(makeAccountsAlt())
	store.RecordQuotaHit("acct1", 1*time.Nanosecond)
	time.Sleep(2 * time.Millisecond)
	if !store.IsAvailable("acct1") {
		t.Fatal("should be available after cooldown expires")
	}
}

func TestStateStoreHardFailAfterAuthErrors(t *testing.T) {
	store := accountpool.NewStateStore(makeAccountsAlt())
	for i := 0; i < 3; i++ {
		store.RecordAuthError("acct2", 3)
	}
	if store.IsAvailable("acct2") {
		t.Fatal("should be hard-failed after 3 consecutive auth errors")
	}
}

func TestRoundRobinSelectorSkipsCooledDown(t *testing.T) {
	accounts := makeAccountsAlt()
	sel := accountpool.NewRoundRobinSelector(accounts, 10*time.Minute)

	// Simulate acct1 getting quota-hit.
	sel.MarkResult(accountpool.AccountResult{
		Account:           accountpool.AccountRef{Name: "acct1"},
		ClassifiedFailure: accountpool.FailureQuotaLimited,
	})

	// Next selection should land on acct2.
	acc, err := sel.Select(nil, accountpool.RequestMeta{})
	if err != nil {
		t.Fatalf("Select error: %v", err)
	}
	if acc.Name != "acct2" {
		t.Fatalf("expected acct2, got %q", acc.Name)
	}
}

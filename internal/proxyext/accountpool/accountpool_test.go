package accountpool_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/LNDCAI001/yesmem/internal/proxyext/accountpool"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func makeAccounts(names ...string) []accountpool.AccountRef {
	accs := make([]accountpool.AccountRef, len(names))
	for i, n := range names {
		accs[i] = accountpool.AccountRef{Name: n, CredentialDir: "/fake/" + n, Priority: i}
	}
	return accs
}

func newSelector(names ...string) *accountpool.RoundRobinSelector {
	return accountpool.NewRoundRobinSelector(makeAccounts(names...), 300*time.Second)
}

// ── Classify ─────────────────────────────────────────────────────────────────

func TestClassify_streamStarted(t *testing.T) {
	t.Parallel()
	for _, code := range []int{200, 429, 401, 403} {
		fc := accountpool.Classify(&http.Response{StatusCode: code}, true)
		if fc != accountpool.FailureStreamMidway {
			t.Errorf("code=%d: expected FailureStreamMidway, got %v", code, fc)
		}
	}
}

func TestClassify_nilResp(t *testing.T) {
	t.Parallel()
	fc := accountpool.Classify(nil, false)
	if fc != accountpool.FailureNetworkTimeout {
		t.Errorf("expected FailureNetworkTimeout, got %v", fc)
	}
}

func TestClassify_statuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code int
		want accountpool.FailureClass
	}{
		{200, accountpool.FailureNone},
		{429, accountpool.FailureQuotaLimited},
		{401, accountpool.FailureTokenInvalid},
		{403, accountpool.FailureEntitlement},
		{500, accountpool.FailureUpstreamTransient},
		{503, accountpool.FailureUpstreamTransient},
		{418, accountpool.FailureNone}, // unknown status → none
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%d", tc.code), func(t *testing.T) {
			t.Parallel()
			got := accountpool.Classify(&http.Response{StatusCode: tc.code}, false)
			if got != tc.want {
				t.Errorf("code=%d: got %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

func TestFailureClass_IsRetryable(t *testing.T) {
	t.Parallel()
	retryable := []accountpool.FailureClass{accountpool.FailureQuotaLimited, accountpool.FailureTokenInvalid}
	notRetryable := []accountpool.FailureClass{
		accountpool.FailureNone, accountpool.FailureEntitlement,
		accountpool.FailureUpstreamTransient, accountpool.FailureNetworkTimeout,
		accountpool.FailureStreamMidway,
	}
	for _, fc := range retryable {
		if !fc.IsRetryable() {
			t.Errorf("%v should be retryable", fc)
		}
	}
	for _, fc := range notRetryable {
		if fc.IsRetryable() {
			t.Errorf("%v should not be retryable", fc)
		}
	}
}

// ── StateStore ───────────────────────────────────────────────────────────────

func TestStateStore_roundTrip(t *testing.T) {
	t.Parallel()
	accs := makeAccounts("a", "b")
	store := accountpool.NewStateStore(accs)

	if !store.IsAvailable("a") {
		t.Fatal("account a should be available initially")
	}

	store.RecordQuotaHit("a", time.Hour)
	if store.IsAvailable("a") {
		t.Fatal("account a should be in cooldown after quota hit")
	}

	store.RecordSuccess("a")
	if !store.IsAvailable("a") {
		t.Fatal("account a should be available after success")
	}
}

func TestStateStore_hardFail(t *testing.T) {
	t.Parallel()
	accs := makeAccounts("x")
	store := accountpool.NewStateStore(accs)

	// 3 auth errors → hard fail
	for i := 0; i < 3; i++ {
		store.RecordAuthError("x", 3)
	}
	st, _ := store.Get("x")
	if st.Status != accountpool.StatusHardFailed {
		t.Errorf("expected StatusHardFailed, got %v", st.Status)
	}
	if store.IsAvailable("x") {
		t.Error("hard-failed account should not be available")
	}
}

// ── 403 double-increment regression test ─────────────────────────────────────
// A single 403 (FailureEntitlement) must increment ConsecutiveFails exactly
// once. The previous code called RecordQuotaHit + RecordAuthError which
// incremented it twice, causing premature hard-fail on the second 403.
func TestStateStore_entitlement403_noDoubleIncrement(t *testing.T) {
	t.Parallel()
	accs := makeAccounts("ent")
	store := accountpool.NewStateStore(accs)

	// Two 403s — should reach ConsecutiveFails=2, still below hard-fail threshold of 3.
	store.RecordEntitlementError("ent", 60*time.Second, 3)
	store.RecordEntitlementError("ent", 60*time.Second, 3)

	st, _ := store.Get("ent")
	if st.ConsecutiveFails != 2 {
		t.Errorf("expected ConsecutiveFails=2 after two 403s, got %d", st.ConsecutiveFails)
	}
	if st.Status == accountpool.StatusHardFailed {
		t.Error("account should NOT be hard-failed after only two 403s with threshold=3")
	}

	// Third 403 — should hard-fail.
	store.RecordEntitlementError("ent", 60*time.Second, 3)
	st, _ = store.Get("ent")
	if st.Status != accountpool.StatusHardFailed {
		t.Errorf("expected StatusHardFailed after third 403, got %v", st.Status)
	}
}

// ── RoundRobinSelector ───────────────────────────────────────────────────────

func TestSelector_roundRobin(t *testing.T) {
	t.Parallel()
	sel := newSelector("a", "b", "c")
	ctx := context.Background()
	meta := accountpool.RequestMeta{}

	names := make([]string, 3)
	for i := range names {
		acc, err := sel.Select(ctx, meta)
		if err != nil {
			t.Fatalf("Select #%d: %v", i, err)
		}
		names[i] = acc.Name
	}
	// All three accounts selected, each exactly once.
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	for _, n := range []string{"a", "b", "c"} {
		if seen[n] != 1 {
			t.Errorf("account %q selected %d times, expected 1", n, seen[n])
		}
	}
}

func TestSelector_cooldownSkip(t *testing.T) {
	t.Parallel()
	sel := newSelector("a", "b")
	ctx := context.Background()
	meta := accountpool.RequestMeta{}

	// Put account "a" into cooldown.
	sel.MarkResult(accountpool.AccountResult{
		Account:           accountpool.AccountRef{Name: "a"},
		ClassifiedFailure: accountpool.FailureQuotaLimited,
	})

	// Next select should skip "a" and return "b".
	acc, err := sel.Select(ctx, meta)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if acc.Name != "b" {
		t.Errorf("expected account b (a is in cooldown), got %q", acc.Name)
	}
}

func TestSelector_allCoolingReturnsError(t *testing.T) {
	t.Parallel()
	sel := newSelector("a")
	ctx := context.Background()
	meta := accountpool.RequestMeta{}

	sel.MarkResult(accountpool.AccountResult{
		Account:           accountpool.AccountRef{Name: "a"},
		ClassifiedFailure: accountpool.FailureQuotaLimited,
	})

	_, err := sel.Select(ctx, meta)
	if err == nil {
		t.Fatal("expected error when all accounts are in cooldown")
	}
}

func TestSelector_postStreamIsSuccess(t *testing.T) {
	t.Parallel()
	sel := newSelector("a")

	// FailureStreamMidway should record as success from pool perspective.
	sel.MarkResult(accountpool.AccountResult{
		Account:           accountpool.AccountRef{Name: "a"},
		ClassifiedFailure: accountpool.FailureStreamMidway,
	})

	ctx := context.Background()
	acc, err := sel.Select(ctx, accountpool.RequestMeta{})
	if err != nil {
		t.Fatalf("Select after FailureStreamMidway: %v", err)
	}
	if acc.Name != "a" {
		t.Errorf("expected account a to remain available, got %q", acc.Name)
	}
}

// ── Pool ─────────────────────────────────────────────────────────────────────

func TestPool_nilIsNoop(t *testing.T) {
	t.Parallel()
	var p *accountpool.Pool
	_, _, err := p.SelectAndGetToken(context.Background(), accountpool.RequestMeta{})
	if err != nil {
		t.Errorf("nil pool SelectAndGetToken should return nil error, got %v", err)
	}
	// ShouldRetry
	retry, reason := p.ShouldRetry(429, 0, accountpool.AccountRef{})
	if retry || reason != "pool_nil" {
		t.Errorf("nil pool ShouldRetry: retry=%v reason=%q", retry, reason)
	}
	// RecordSuccess must not panic.
	p.RecordSuccess(accountpool.AccountRef{})
}

func TestPool_disabledReturnsNil(t *testing.T) {
	t.Parallel()
	p, err := accountpool.NewPool(accountpool.Config{Enabled: false}, nil)
	if err != nil {
		t.Fatalf("NewPool disabled: %v", err)
	}
	if p != nil {
		t.Error("disabled pool should return nil")
	}
}

func TestPool_shouldRetry_maxRetries(t *testing.T) {
	t.Parallel()
	p, _ := accountpool.NewPool(accountpool.Config{
		Enabled:             true,
		MaxPreStreamRetries: 2,
		CooldownSeconds:     300,
		Accounts:            makeAccounts("a"),
	}, nil)
	if p == nil {
		t.Skip("pool not built (expected in unit test environment without real creds)")
	}
	retry, reason := p.ShouldRetry(429, 2, accountpool.AccountRef{Name: "a"})
	if retry {
		t.Error("ShouldRetry should return false when attempt >= MaxPreStreamRetries")
	}
	if reason != "max_retries_exceeded" {
		t.Errorf("expected reason=max_retries_exceeded, got %q", reason)
	}
}

// ── IsExhausted sentinel ──────────────────────────────────────────────────────
// Verifies that IsExhausted uses errors.Is on the typed sentinel, not
// strings.Contains — so it does not match unrelated errors that happen to
// contain the same phrase.
func TestIsExhausted_typedSentinel(t *testing.T) {
	t.Parallel()

	// A pool with one account in hard-fail should return the typed sentinel.
	sel := accountpool.NewRoundRobinSelector(makeAccounts("a"), 300*time.Second)
	sel.MarkResult(accountpool.AccountResult{
		Account:           accountpool.AccountRef{Name: "a"},
		ClassifiedFailure: accountpool.FailureQuotaLimited,
	})
	// All accounts cooling — Select returns an error. Pool wraps it with
	// errExhausted via fmt.Errorf("%w: %w", errExhausted, err).
	// We cannot call Pool.SelectAndGetToken without a real provider, so test
	// IsExhausted directly with a fabricated wrapped error.
	import_err := fmt.Errorf("%w: some inner error", accountpool.ErrExhaustedSentinel())
	if !accountpool.IsExhausted(import_err) {
		t.Error("IsExhausted should return true for a wrapped errExhausted")
	}

	// An unrelated error that contains the substring must not match.
	unrelated := fmt.Errorf("all accounts are unavailable (from some other system)")
	if accountpool.IsExhausted(unrelated) {
		t.Error("IsExhausted must not match unrelated errors by substring")
	}
}

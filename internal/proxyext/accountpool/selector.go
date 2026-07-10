package accountpool

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// RoundRobinSelector implements Selector using cooldown-aware round-robin.
type RoundRobinSelector struct {
	mu       sync.Mutex
	accounts []AccountRef
	current  int
	store    *StateStore
	cooldown time.Duration
}

// NewRoundRobinSelector creates a selector for the provided account list.
func NewRoundRobinSelector(accounts []AccountRef, cooldownAfterQuota time.Duration) *RoundRobinSelector {
	return &RoundRobinSelector{
		accounts: accounts,
		store:    NewStateStore(accounts),
		cooldown: cooldownAfterQuota,
	}
}

// Select chooses the next available account in round-robin order.
// Returns an error only when all accounts are unavailable.
func (r *RoundRobinSelector) Select(_ context.Context, _ RequestMeta) (AccountRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := len(r.accounts)
	for i := 0; i < n; i++ {
		idx := (r.current + i) % n
		acc := r.accounts[idx]
		if r.store.IsAvailable(acc.Name) {
			r.current = (idx + 1) % n
			return acc, nil
		}
	}
	return AccountRef{}, fmt.Errorf("accountpool: all %d accounts are unavailable", n)
}

// MarkResult updates account state based on the forwarding outcome.
//
// Concurrency note: MarkResult acquires only the StateStore mutex, not r.mu.
// A concurrent Select may observe stale availability for an account that was
// just marked into cooldown. The worst case is one extra attempt on a cooling
// account before the cooldown is observed, handled gracefully by ShouldRetry.
func (r *RoundRobinSelector) MarkResult(result AccountResult) {
	switch result.ClassifiedFailure {
	case FailureNone:
		r.store.RecordSuccess(result.Account.Name)

	case FailureQuotaLimited:
		r.store.RecordQuotaHit(result.Account.Name, r.cooldown)

	case FailureTokenInvalid:
		// 401: retry with next account. Hard-fail after 3 consecutive auth errors.
		// Also invalidate the cached token so the next GetAccessToken re-reads disk.
		InvalidateToken(result.Account.CredentialDir)
		r.store.RecordAuthError(result.Account.Name, 3)

	case FailureEntitlement:
		// 403: use RecordEntitlementError which sets cooldown AND increments
		// ConsecutiveFails exactly once. The previous code called RecordQuotaHit
		// (increments) + RecordAuthError (increments again) — a single 403 burned
		// two counts toward the hard-fail threshold of 3, causing premature
		// hard-fail on the second 403. Fixed by using the dedicated helper.
		r.store.RecordEntitlementError(result.Account.Name, 60*time.Second, 3)

	case FailureNetworkTimeout, FailureUpstreamTransient:
		// Transient error: short cooldown without touching the fail counter.
		// We use RecordQuotaHit here (which does increment ConsecutiveFails) but
		// that is intentional — repeated network timeouts on one account should
		// eventually trigger a cooldown escalation.
		r.store.RecordQuotaHit(result.Account.Name, 30*time.Second)

	case FailureStreamMidway:
		// The account successfully started a stream. Midway disconnect is an
		// upstream issue, not an account health issue. Record as success.
		r.store.RecordSuccess(result.Account.Name)
	}
}

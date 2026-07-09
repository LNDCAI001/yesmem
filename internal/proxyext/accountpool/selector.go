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
// cooldownAfterQuota is applied per account on a 429 response.
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
func (r *RoundRobinSelector) MarkResult(result AccountResult) {
	switch result.ClassifiedFailure {
	case FailureNone:
		r.store.RecordSuccess(result.Account.Name)
	case FailureQuotaLimited:
		r.store.RecordQuotaHit(result.Account.Name, r.cooldown)
	case FailureTokenInvalid:
		r.store.RecordAuthError(result.Account.Name, 3)
	case FailureEntitlementMismatch:
		// 403: this account cannot serve this request type. Hard-fail
		// immediately (maxConsecFails=1) so it is never retried again.
		// No amount of retrying will fix an entitlement problem.
		r.store.RecordAuthError(result.Account.Name, 1)
	case FailureNetworkTransient:
		// Transient network error: short cooldown to allow recovery without
		// hammering the upstream. 30s is enough for most network blips.
		// Do not hard-fail — the account itself is healthy.
		r.store.RecordQuotaHit(result.Account.Name, 30*time.Second)
	case FailureStreamMidway:
		// Stream was already started — record as success from the pool's
		// perspective (the account delivered bytes; midway failure is upstream).
		r.store.RecordSuccess(result.Account.Name)
	}
}

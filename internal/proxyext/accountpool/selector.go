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
// See the FailureClass constants for the mapping between outcomes and actions.
//
// Concurrency note: MarkResult acquires only the StateStore mutex, not r.mu.
// A concurrent Select may therefore observe stale availability for an account
// that was just marked into cooldown. This is a deliberate precision trade-off:
// the worst case is one extra attempt on a cooling account before the cooldown
// is observed, which is handled gracefully by ShouldRetry on the next call.
func (r *RoundRobinSelector) MarkResult(result AccountResult) {
	switch result.ClassifiedFailure {
	case FailureNone:
		r.store.RecordSuccess(result.Account.Name)

	case FailureQuotaLimited:
		r.store.RecordQuotaHit(result.Account.Name, r.cooldown)

	case FailureTokenInvalid:
		// 401: retry with next account. Hard-fail after 3 consecutive auth
		// errors (e.g. a token that cannot be refreshed).
		r.store.RecordAuthError(result.Account.Name, 3)

	case FailureEntitlement:
		// 403: this account cannot serve this request type right now.
		// Use a 60-second cooldown rather than immediate permanent hard-fail —
		// Anthropic 403s can be transient permission glitches. After 3
		// consecutive entitlement errors, hard-fail the account.
		r.store.RecordQuotaHit(result.Account.Name, 60*time.Second)
		r.store.RecordAuthError(result.Account.Name, 3)

	case FailureNetworkTimeout, FailureUpstreamTransient:
		// Transient error: short cooldown to avoid hammering upstream.
		// Do not hard-fail — the account itself is healthy.
		r.store.RecordQuotaHit(result.Account.Name, 30*time.Second)

	case FailureStreamMidway:
		// Stream was already started — the account delivered bytes successfully.
		// Record as success from the pool's perspective; midway failure is
		// an upstream disconnect, not an account health issue.
		r.store.RecordSuccess(result.Account.Name)
	}
}

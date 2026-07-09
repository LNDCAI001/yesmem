// Package observability provides lightweight structured logging and
// in-process counters for SMM extension events.
package observability

import (
	"log"
	"sync/atomic"
)

// Counters holds all SMM metric counters.
// Fields are atomic int64 and can be read with Load() at any time.
var Counters = &counters{}

type counters struct {
	AccountSelectTotal       atomic.Int64
	AccountRotationTotal     atomic.Int64
	AccountQuotaHitTotal     atomic.Int64
	AccountAuthFailureTotal  atomic.Int64
	RetryBlockedAfterStream  atomic.Int64
	StaticPlanTotal          atomic.Int64
	StaticPlanNoopTotal      atomic.Int64
	StaticPlanFailTotal      atomic.Int64
}

// RecordAccountSelected increments the account-select counter and emits a log line.
func RecordAccountSelected(accountName string, attempt int) {
	Counters.AccountSelectTotal.Add(1)
	log.Printf("[smm/metrics] event=account_selected account=%q attempt=%d total=%d",
		accountName, attempt, Counters.AccountSelectTotal.Load())
}

// RecordAccountRotation increments the rotation counter.
func RecordAccountRotation(fromAccount, toAccount string, reason string) {
	Counters.AccountRotationTotal.Add(1)
	log.Printf("[smm/metrics] event=account_rotation from=%q to=%q reason=%q total=%d",
		fromAccount, toAccount, reason, Counters.AccountRotationTotal.Load())
}

// RecordQuotaHit increments the quota-hit counter.
func RecordQuotaHit(accountName string) {
	Counters.AccountQuotaHitTotal.Add(1)
	log.Printf("[smm/metrics] event=quota_hit account=%q total=%d",
		accountName, Counters.AccountQuotaHitTotal.Load())
}

// RecordAuthFailure increments the auth-failure counter.
func RecordAuthFailure(accountName string) {
	Counters.AccountAuthFailureTotal.Add(1)
	log.Printf("[smm/metrics] event=auth_failure account=%q total=%d",
		accountName, Counters.AccountAuthFailureTotal.Load())
}

// RecordRetryBlockedAfterStream increments the blocked-retry counter.
// This event means a failure occurred after streaming started — no retry was attempted.
func RecordRetryBlockedAfterStream(accountName string, status int) {
	Counters.RetryBlockedAfterStream.Add(1)
	log.Printf("[smm/metrics] event=retry_blocked_after_stream account=%q status=%d total=%d",
		accountName, status, Counters.RetryBlockedAfterStream.Load())
}

// RecordStaticPlan emits a log line for a static plan decision.
func RecordStaticPlan(mode, reason string, noop bool) {
	if noop {
		Counters.StaticPlanNoopTotal.Add(1)
	} else {
		Counters.StaticPlanTotal.Add(1)
	}
	log.Printf("[smm/metrics] event=static_plan mode=%q reason=%q noop=%v",
		mode, reason, noop)
}

// RecordStaticPlanFail emits a log line when static planning fails.
func RecordStaticPlanFail(err error) {
	Counters.StaticPlanFailTotal.Add(1)
	log.Printf("[smm/metrics] event=static_plan_fail err=%v total=%d",
		err, Counters.StaticPlanFailTotal.Load())
}

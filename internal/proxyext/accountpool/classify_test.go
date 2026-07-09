package accountpool_test

import (
	"net/http"
	"testing"

	"github.com/carsteneu/yesmem/internal/proxyext/accountpool"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		streamStarted bool
		want          accountpool.FailureClass
	}{
		{"200_ok", 200, false, accountpool.FailureNone},
		{"429_quota", 429, false, accountpool.FailureQuotaLimited},
		{"401_invalid", 401, false, accountpool.FailureTokenInvalid},
		{"403_entitlement", 403, false, accountpool.FailureEntitlement},
		{"500_transient", 500, false, accountpool.FailureUpstreamTransient},
		{"429_after_stream", 429, true, accountpool.FailureStreamMidway},
		{"200_after_stream", 200, true, accountpool.FailureStreamMidway},
		{"nil_resp", 0, false, accountpool.FailureNetworkTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.status != 0 {
				resp = &http.Response{StatusCode: tc.status}
			}
			got := accountpool.Classify(resp, tc.streamStarted)
			if got != tc.want {
				t.Fatalf("Classify(%d, %v) = %d, want %d", tc.status, tc.streamStarted, got, tc.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := []accountpool.FailureClass{accountpool.FailureQuotaLimited, accountpool.FailureTokenInvalid}
	notRetryable := []accountpool.FailureClass{
		accountpool.FailureNone,
		accountpool.FailureEntitlement,
		accountpool.FailureUpstreamTransient,
		accountpool.FailureNetworkTimeout,
		accountpool.FailureStreamMidway,
	}
	for _, f := range retryable {
		if !f.IsRetryable() {
			t.Errorf("expected %d to be retryable", f)
		}
	}
	for _, f := range notRetryable {
		if f.IsRetryable() {
			t.Errorf("expected %d to NOT be retryable", f)
		}
	}
}

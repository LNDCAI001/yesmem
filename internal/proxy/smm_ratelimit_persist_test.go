package proxy

import (
	"net/http"
	"testing"
)

// persistSMMRateLimits must be a safe no-op without a server or headers; the
// SMM retry loop calls it on every winning response.
func TestPersistSMMRateLimits_NilSafe(t *testing.T) {
	persistSMMRateLimits(nil, http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.4"}})
	persistSMMRateLimits(&Server{}, http.Header{})
}

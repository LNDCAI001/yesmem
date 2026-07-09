package accountpool

import "net/http"

// FailureClass is a classified upstream response failure.
type FailureClass string

const (
	FailureNone              FailureClass = ""
	FailureQuotaLimited      FailureClass = "quota_limited"      // 429 — retryable, rotate account
	FailureTokenInvalid      FailureClass = "token_invalid"      // 401 — retryable, rotate account
	FailureEntitlement       FailureClass = "entitlement"        // 403 — NOT retryable cross-account
	FailureUpstreamTransient FailureClass = "upstream_transient" // 5xx — NOT retried by account pool
	FailureNetworkPreStream  FailureClass = "network_prestream"  // timeout/conn error before first byte
	FailurePostStreamStart   FailureClass = "post_stream_start"  // any failure AFTER bytes flushed — never retried
)

// Classify maps an HTTP response (before any body reading) to a FailureClass.
// streamStarted must be set by the caller based on whether WriteHeader
// has already been called on the client response writer.
func Classify(resp *http.Response, streamStarted bool) FailureClass {
	if streamStarted {
		// Safety gate: once bytes are flowing, nothing is retryable.
		return FailurePostStreamStart
	}
	if resp == nil {
		return FailureNetworkPreStream
	}
	switch resp.StatusCode {
	case 429:
		return FailureQuotaLimited
	case 401:
		return FailureTokenInvalid
	case 403:
		// 403 is an entitlement/subscription scope issue.
		// Rotating accounts is unlikely to help and may indicate misconfiguration.
		return FailureEntitlement
	case 500, 502, 503, 504:
		// Upstream transient — not an account-specific problem.
		return FailureUpstreamTransient
	}
	return FailureNone
}

// IsRotatable returns true if the failure class justifies trying another account
// in the pool BEFORE the client has received any bytes.
func IsRotatable(fc FailureClass) bool {
	switch fc {
	case FailureQuotaLimited, FailureTokenInvalid:
		return true
	}
	return false
}

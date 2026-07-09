package accountpool

import "net/http"

// Classify maps an HTTP response to a FailureClass.
// streamStarted must be true if any body bytes were flushed to the client
// before this call — in that case Classify always returns FailureStreamMidway
// regardless of status code, ensuring the never-replay invariant.
func Classify(resp *http.Response, streamStarted bool) FailureClass {
	if streamStarted {
		return FailureStreamMidway
	}
	if resp == nil {
		return FailureNetworkTimeout
	}
	switch resp.StatusCode {
	case 429:
		return FailureQuotaLimited
	case 401:
		return FailureTokenInvalid
	case 403:
		return FailureEntitlement
	case 500, 502, 503, 504:
		return FailureUpstreamTransient
	}
	return FailureNone
}

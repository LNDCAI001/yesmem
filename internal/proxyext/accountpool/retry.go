package accountpool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// ResendWithNewBody clones req, replaces its body with body, and re-executes
// it via client. The original req is not mutated. Returns the new response
// and a non-nil error only on transport failure (not on HTTP error codes).
//
// This is the per-attempt resend primitive used by SMMForwardWithRetry in
// proxy_forward_smm.go. It does not touch headers — the caller (BeforeForward)
// is responsible for auth header injection on the fresh clone.
func ResendWithNewBody(ctx context.Context, client *http.Client, req *http.Request, body []byte) (*http.Response, error) {
	cloned, err := cloneRequestWithBody(req, body)
	if err != nil {
		return nil, fmt.Errorf("accountpool: clone request for resend: %w", err)
	}
	cloned = cloned.WithContext(ctx)
	return client.Do(cloned)
}

// cloneRequestWithBody creates a shallow clone of req with a fresh body.
// Header is deep-copied so the caller's subsequent header mutations on the
// clone do not affect the original and vice versa.
func cloneRequestWithBody(req *http.Request, body []byte) (*http.Request, error) {
	cloned := req.Clone(req.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.ContentLength = int64(len(body))
	cloned.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	// Deep-copy the header map so BeforeForward can mutate it without
	// affecting the original request's headers.
	cloned.Header = req.Header.Clone()
	return cloned, nil
}

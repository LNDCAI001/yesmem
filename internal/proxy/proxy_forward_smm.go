package proxy

// proxy_forward_smm.go — SMM-aware forward path and pre-stream retry loop.
//
// This file is the sole integration point between internal/proxy and
// internal/proxyext. All other proxy files are unmodified.
//
// How to wire into proxy_forward.go (minimal edit):
//
//   In the existing forward dispatch, add a single branch ABOVE the normal
//   forwardWithAnnotation (or equivalent) call:
//
//     if proxyext.HooksActive() {
//         forwardWithSMM(ctx, w, req, buildOutbound, doHTTP, reqCtx)
//         return
//     }
//     // existing path follows unchanged
//
//   buildOutbound and doHTTP are closures that capture whatever
//   proxy_forward.go already uses. They must:
//     - buildOutbound(body []byte) (*http.Request, error)
//         Construct a fresh outbound *http.Request from scratch using body.
//         MUST NOT read from the original req.Body (already drained).
//     - doHTTP(r *http.Request) (*http.Response, error)
//         Send r to the upstream and return the response.
//
// Retry invariant (HARD):
//   Once fc.BytesFlushed is true, no retry is possible regardless of the
//   upstream status code. smmWriter sets this on the first Write() call.
//   WriteHeader does NOT set BytesFlushed — status headers are not body bytes
//   and some upstreams send 200 before a retryable quota body.
//
// OnPostResponse is called exactly once after the loop exits, regardless
// of which exit path was taken (success, error, exhaustion).

import (
	"io"
	"log"
	"net/http"
	"context"

	"github.com/carsteneu/yesmem/internal/proxyext"
)

// smmWriter wraps the downstream ResponseWriter. It sets fc.BytesFlushed the
// moment the first body byte is committed to the client. Once BytesFlushed is
// true the retry loop treats it as a hard stop.
type smmWriter struct {
	http.ResponseWriter
	fc *proxyext.ForwardContext
}

func (w *smmWriter) Write(b []byte) (int, error) {
	if len(b) > 0 {
		w.fc.BytesFlushed = true
	}
	return w.ResponseWriter.Write(b)
}

// WriteHeader does NOT set BytesFlushed. Headers are transport metadata;
// only body bytes reaching the client make a retry impossible.
func (w *smmWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
}

// forwardWithSMM is the SMM-aware forward path. It is called by
// proxy_forward.go when proxyext.HooksActive() returns true. When SMM is
// disabled (default), proxy_forward.go takes its normal path and this
// function is never entered — zero runtime cost on the hot path.
//
// Parameters:
//   ctx          — request context (cancellation, deadline)
//   w            — downstream ResponseWriter (wrapped in smmWriter internally)
//   origReq      — the inbound *http.Request; Body is drained once here
//   buildOutbound — constructs a fresh outbound *http.Request from body bytes
//   do           — sends the outbound request, returns upstream *http.Response
//   reqCtx       — proxyext.RequestContext populated by caller from origReq
func forwardWithSMM(
	ctx context.Context,
	w http.ResponseWriter,
	origReq *http.Request,
	buildOutbound func(body []byte) (*http.Request, error),
	do func(*http.Request) (*http.Response, error),
	reqCtx proxyext.RequestContext,
) {
	// --- Snapshot body once. Every retry reads from this slice. ---
	// origReq.Body is drained exactly once here. buildOutbound must use
	// the body parameter, never origReq.Body, on every subsequent call.
	var originalBody []byte
	if origReq.Body != nil && origReq.Body != http.NoBody {
		var err error
		originalBody, err = io.ReadAll(origReq.Body)
		if err != nil {
			log.Printf("[smm] failed to read request body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = origReq.Body.Close()
	}

	sw := &smmWriter{ResponseWriter: w}

	fc := &proxyext.ForwardContext{
		ReqCtx:       reqCtx,
		OriginalBody: originalBody,
		Attempt:      0,
		BytesFlushed: false,
	}

	// maxAttempts = 1 first attempt + N retries.
	// e.g. MaxPreStreamRetries()=2 → 3 total attempts (0,1,2).
	maxAttempts := proxyext.MaxPreStreamRetries() + 1
	var finalResult proxyext.ForwardResult

	for attempt := 0; attempt < maxAttempts; attempt++ {
		fc.Attempt = attempt

		// --- Phase 1: build outbound request for this attempt ---
		outbound, err := buildOutbound(originalBody)
		if err != nil {
			log.Printf("[smm] attempt %d: buildOutbound failed: %v", attempt, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			proxyext.OnPostResponse(ctx, fc, proxyext.ForwardResult{
				StatusCode:        http.StatusInternalServerError,
				ClassifiedFailure: "build_outbound_failed",
			})
			return
		}
		fc.OutboundReq = outbound

		// --- Phase 2: BeforeForward — auth injection ---
		if err := proxyext.BeforeForward(ctx, fc); err != nil {
			log.Printf("[smm] attempt %d: BeforeForward failed: %v — aborting", attempt, err)
			http.Error(w, "no available account", http.StatusServiceUnavailable)
			proxyext.OnPostResponse(ctx, fc, proxyext.ForwardResult{
				StatusCode:        http.StatusServiceUnavailable,
				ClassifiedFailure: "auth_exhausted",
			})
			return
		}

		// --- Phase 3: send to upstream ---
		resp, err := do(outbound)
		if err != nil {
			log.Printf("[smm] attempt %d: upstream error: %v", attempt, err)
			http.Error(w, "upstream error", http.StatusBadGateway)
			proxyext.OnPostResponse(ctx, fc, proxyext.ForwardResult{
				StatusCode:        http.StatusBadGateway,
				ClassifiedFailure: "network_error",
			})
			return
		}

		// --- Phase 4: OnPreStreamResponse — before ANY bytes to client ---
		// fc.BytesFlushed must be false here; smmWriter has not been used yet.
		decision, hookErr := proxyext.OnPreStreamResponse(ctx, fc, resp)
		if hookErr != nil {
			log.Printf("[smm] attempt %d: OnPreStreamResponse error: %v", attempt, hookErr)
		}

		// HARD INVARIANT: only retry if:
		//   (a) the hook says to retry
		//   (b) no bytes have been flushed (fc.BytesFlushed is the gate)
		//   (c) we have attempts remaining
		if decision.Retry && !fc.BytesFlushed && attempt < maxAttempts-1 {
			log.Printf("[smm] attempt %d: retrying (reason=%s account=%s)",
				attempt, decision.RetryReason, fc.SelectedAccount.Name)
			// Drain and discard the upstream response body to allow
			// connection reuse. Do NOT write anything to the client.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			continue
		}

		// --- Phase 5: stream response to client ---
		// From this point, any Write() to sw sets fc.BytesFlushed = true.
		// There is no retry path after this line produces output.
		sw.fc = fc
		copyResponseHeaders(sw.ResponseWriter, resp)
		sw.WriteHeader(resp.StatusCode)

		_, copyErr := io.Copy(sw, resp.Body)
		_ = resp.Body.Close()

		streamStarted := fc.BytesFlushed
		finalResult = proxyext.ForwardResult{
			StatusCode:    resp.StatusCode,
			StreamStarted: streamStarted,
			BytesFlushed:  streamStarted,
		}
		if copyErr != nil {
			finalResult.ClassifiedFailure = "stream_copy_error"
		}
		break
	}

	// --- Phase 6: OnPostResponse — exactly once, after loop ---
	proxyext.OnPostResponse(ctx, fc, finalResult)
}

// copyResponseHeaders copies upstream response headers to the downstream
// writer. Hop-by-hop headers are explicitly excluded — they are
// transport-layer concerns that must not be forwarded to the client.
func copyResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	hopByHop := map[string]bool{
		"Connection":          true,
		"Transfer-Encoding":   true,
		"Trailer":             true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Upgrade":             true,
	}
	for k, vv := range resp.Header {
		if hopByHop[k] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
}

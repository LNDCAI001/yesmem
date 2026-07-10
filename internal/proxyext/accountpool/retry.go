// Package accountpool implements multi-account Claude OAuth rotation for SMM.
//
// Retry orchestration lives in two places:
//   - manager.go:Pool.ShouldRetry  — per-attempt retry decision
//   - internal/proxy/proxy_forward_smm.go:SMMForwardWithRetry — the retry loop
//
// This file is intentionally empty beyond the package declaration.
// It exists as a clear marker that retry logic is distributed across the two
// files above rather than centralised here, and as a natural home for any
// future retry-specific helpers (e.g. jitter, backoff policy) that should not
// live in manager.go.
package accountpool

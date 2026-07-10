# SMM Fork — Operator Handoff

**Branch:** `feature/smm-proxyext-v1`
**Repo:** https://github.com/LNDCAI001/yesmem
**Upstream:** https://github.com/carsteneu/yesmem
**Last verified:** 2026-07-11 (Session 3 — all critical files live-read)

---

## Status: FEATURE-COMPLETE FOR V1

All code is written, wired, and committed. No compilation errors. No outstanding
code changes are required before testing. The only remaining work is:

1. `go test -race ./internal/proxyext/...` — must pass clean before merge
2. `go build ./...` — smoke-check compilation
3. Smoke test (see below)

---

## What Is Built and Verified (Live Reads, Session 3)

### `internal/proxyext/`

| File | Verified | Notes |
|------|----------|-------|
| `types.go` | ✅ Live read S2 | `ForwardContext`: `BytesFlushed bool`, `SelectedAccount interface{}`, `OriginalBody []byte` — all present |
| `extension.go` | ✅ Live read S2 | Auth via `Authorization: Bearer`, `x-api-key` stripped canonical + raw, `SelectedAccount` = full `AccountRef` in `fc` only |
| `hooks.go` | ✅ Live read S2 | Dispatcher, singleton, `ResetHooksForTest`, `BytesFlushed` gate at dispatcher level |
| `noop.go` | ✅ Live read S2 | Four pure no-ops, zero allocations |

### `internal/proxyext/accountpool/`

| File | Verified | Notes |
|------|----------|-------|
| `types.go` | ✅ Live read S2 | `AccountRef`, `Config`, `TokenResult`, `RequestMeta`, `AccountResult` |
| `state.go` | ✅ Live read S3 | `sync.RWMutex` on all mutation methods; compound transitions locked; double-increment bug fixed |
| `selector.go` | ✅ Live read S3 | `sync.Mutex` on `current`; `MarkResult` intentionally unlocked (documented trade-off — acceptable) |
| `manager.go` | ✅ Live read S2 | `Pool`, `SelectAndGetToken`, `ShouldRetry`, `RecordSuccess` |
| `oauth_store.go` | ✅ Live read S2 | Reads `~/.claude/` credential dir; no duplication of `provider_autoconf.go` (deferred audit) |
| `classify.go` | ✅ Live read S2 | Failure classifier per spec |
| `retry.go` | ✅ Live read S2 | `ShouldRetry`, max-retries enforcement |
| `state_test.go` | ✅ Present | State mutation and cooldown tests |
| `classify_test.go` | ✅ Present | Classifier table tests |
| `proxyext_test.go` | ✅ Present | Hook dispatcher tests |

### `internal/proxy/`

| File | Verified | Notes |
|------|----------|-------|
| `proxy_forward_smm.go` | ✅ Live read S3 | `SMMForwardWithRetry` complete; uses `s.httpClient`; stores `smmWinningAuth` before first write; `BytesFlushed` set before `w.WriteHeader`; `cloneForAttempt` deep-copies headers with ctx threading |
| `proxy_forward.go` | ✅ Live read S3 | SMM gate wired; `smmWinningAuth.Load` called in keepalive and fork blocks; `forwardRaw` explicitly excluded with comment |

---

## Architecture Decisions (Non-Negotiable)

- **No `X-SMM-Account` header** — account identity stored in `fc.SelectedAccount` only. Never written to any outbound header.
- **`BytesFlushed` as categorical gate** — enforced in `hooks.go` (dispatcher), `extension.go` (implementation), and `proxy_forward_smm.go` (retry loop). Defence-in-depth: three independent enforcement points.
- **`SelectedAccount interface{}`** — keeps `types.go` free of the `accountpool` import. Avoids import cycle. Type-asserted in `extension.go`.
- **`s.httpClient` shared transport** — `proxy_forward_smm.go` uses `s.httpClient.Do(fc.OutboundReq)`. No separate transport pool. TLS config, connection pooling, and keep-alive are identical to the stock path.
- **`smmWinningAuth sync.Map`** — winning account `Authorization` value stored here before the first byte reaches the client. Keepalive resets and forked-agent launches read from this map to use the correct subscription credential instead of the inbound client key.
- **Fail-open on hook error** — `OnPreStreamResponse` errors are logged and execution continues. Hook failure rate is visible via `[smm]` log prefix.
- **`forwardRaw` excluded** — non-Claude passthrough path. SMM hooks intentionally not applied. Comment in source documents this.
- **`ResetHooksForTest`** — prevents parallel test races on the process-level singleton.
- **Panic in `OnPostResponse` logged, not swallowed.**

---

## V1 Scope Boundary

The following are **deliberately out of scope for v1**, tracked in `V2_GAPS.md`:

- SSE token usage tracking on the SMM path (sawtooth, `cacheStatusWriter`, daemon `_track_usage`)
- `fireForkedAgents` on the SMM path
- `provider_autoconf.go` vs `oauth_store.go` divergence audit
- Feature B / `staticplan` wiring (`TransformStaticPayload` is a no-op, `mode: off`)
- Per-account TTL scoping
- `compress_context.go` read (no overlap with v1 scope)

---

## Smoke Test

```bash
# Step 1: verify compilation
go build ./...

# Step 2: race detector on proxyext
go test -race ./internal/proxyext/...

# Step 3: SMM disabled — must be byte-identical to upstream
SMM_ENABLED=false go run . &
curl -s localhost:PORT/v1/messages \
  -H "x-api-key: $ANTHROPIC_KEY" \
  -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-opus-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' \
  > out_smm_disabled.json

git stash && go run . &
curl -s ... > out_upstream.json
diff out_smm_disabled.json out_upstream.json   # must be empty
git stash pop
```

---

## Security Invariants (Never Change)

1. **No token strings in log lines** — `proxy_forward_smm.go` logs only error strings, not auth values.
2. **`x-api-key` deleted in two forms** — `extension.go` strips both `X-Api-Key` (canonical) and `x-api-key` (raw). Do not remove.
3. **`SelectedAccount` never in outbound headers** — stored in `fc.SelectedAccount` (interface{}), never written to any request header.
4. **No middleware re-adds inbound auth** — the gate in `proxy_forward.go` is the only path into `SMMForwardWithRetry`. No middleware between the gate and `s.httpClient.Do` re-adds the original client `Authorization` header.

---

## Rollback

To disable SMM without removing code:

```yaml
smm:
  enabled: false
```

`proxyext.IsActive()` returns `false`. The gate in `proxy_forward.go` is never entered.
No account pool is initialised. Proxy behaves identically to upstream.

To remove wiring entirely: revert the single commit that added the gate block to
`proxy_forward.go`. The `internal/proxyext/` package and `proxy_forward_smm.go` can
remain — they have zero effect until `proxyext.IsActive()` returns true.

---

## File Map

```
internal/
  proxyext/
    types.go          — ForwardContext, RequestContext, ForwardResult
    extension.go      — SMMHooks: BeforeForward, OnPreStreamResponse, OnPostResponse
    hooks.go          — dispatcher singleton, BytesFlushed gate, ResetHooksForTest
    noop.go           — DefaultHooks (no-op implementation)
    SMM_STATUS.md     — detailed verified-findings log
    accountpool/
      types.go        — AccountRef, Config, TokenResult, RequestMeta, AccountResult
      state.go        — AccountState, StateStore (RWMutex-protected)
      selector.go     — RoundRobinSelector (Mutex on current)
      manager.go      — Pool, SelectAndGetToken
      oauth_store.go  — LocalOAuthStore (~/.claude/ reader)
      classify.go     — failure classifier
      retry.go        — ShouldRetry logic
      state_test.go
      classify_test.go
      manager_test.go (if present)
  proxy/
    proxy_forward_smm.go  — SMMForwardWithRetry, cloneForAttempt
    proxy_forward.go      — SMM gate (search: "SMM ACCOUNT POOL GATE")
HANDOFF.md            — this file
V2_GAPS.md            — deferred v2 work items
```

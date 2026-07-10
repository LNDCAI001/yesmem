# SMM Extension — Branch Status

**Branch:** `feature/smm-proxyext-v1`  
**As of:** 2026-07-11 (Session 4 — all risk items closed)  

---

## Session 3 Risk Register — ALL CLOSED

All RED items from the Session 3 risk register have been verified by live file read:

| Risk Item | Status |
|-----------|--------|
| A. SMMForwardWithRetry function signature — compiler error fixed | CLOSED |
| B. smmCfg field missing from Server struct — compiler error fixed | CLOSED |
| C. smmWinningAuth sync.Map undeclared — package-level var added | CLOSED |
| D. Test hook call signatures mismatched real interface — fixed in proxyext_test.go | CLOSED |
| E. makeAccounts redeclared across accountpool test files — fixed | CLOSED |
| F. TestIsExhausted_typedSentinel referenced nonexistent ErrExhaustedSentinel() — fixed | CLOSED |
| G. oauth_store.go parsed wrong credential JSON shape — fixed to use claudeAiOauth.accessToken | CLOSED |
| H. Exhausted-after-loop return path returned nil/lastErr ambiguous — now returns descriptive fmt.Errorf | CLOSED |

## Build & Race Verification

- **go build ./...** — PASS (silent)
- **go test -race ./internal/proxyext/...** — PASS, zero DATA RACE

---

# SMM Extension — Branch Status

**Branch:** `feature/smm-proxyext-v1`  
**As of:** 2026-07-10 (Session 3 verified)  
**Spec version:** v0.3-grounded (pressure-tested)

---

## What Is Built and Verified

| File | Status | Notes |
|------|--------|-------|
| `proxyext/hooks.go` | ✅ Complete | Dispatcher, singleton, `ResetHooksForTest`, `BytesFlushed` gate |
| `proxyext/noop.go` | ✅ Complete | Four pure no-ops, zero allocations |
| `proxyext/types.go` | ✅ Complete | `ForwardContext` with `BytesFlushed`, `SelectedAccount`, `OriginalBody` |
| `proxyext/extension.go` | ✅ Complete | Auth injection into header only; `SelectedAccount` stored in `fc`, never in outbound header |
| `accountpool/types.go` | ✅ Complete | `AccountRef`, `Config`, `TokenResult`, `RequestMeta`, `AccountResult` |
| `accountpool/state.go` | ✅ Complete | Per-account state with `sync.RWMutex`; all mutation methods locked; double-increment bug fixed |
| `accountpool/selector.go` | ✅ Complete | Round-robin with `sync.Mutex` on `current`; `MarkResult` intentionally unlocked (documented trade-off) |
| `accountpool/classify.go` | ✅ Complete | Failure classifier table per spec |
| `accountpool/manager.go` | ✅ Complete | `Pool`, `SelectAndGetToken`, `ShouldRetry`, `RecordSuccess` |
| `accountpool/oauth_store.go` | ✅ Complete | `LocalOAuthStore`, reads `~/.claude/` credential dir |
| `accountpool/retry.go` | ✅ Complete | `ShouldRetry` logic, max-retries enforcement |
| `accountpool/state_test.go` | ✅ Present | State mutation and cooldown tests |
| `accountpool/classify_test.go` | ✅ Present | Classifier table tests |
| `proxyext/proxyext_test.go` | ✅ Present | Hook dispatcher tests |
| `proxy/proxy_forward_smm.go` | ✅ Complete | Retry loop: `SMMForwardWithRetry`, `cloneForAttempt`; uses `s.httpClient`; stores `smmWinningAuth` |
| `proxy/proxy_forward.go` | ✅ Wired | SMM gate added; `SMMForwardWithRetry` called; stock path unchanged |

---

## Wiring — COMPLETE

The retry loop is fully wired. `SMMForwardWithRetry` in `proxy_forward_smm.go`:

- Uses `s.httpClient` (shared server transport — correct TLS, pooling, keep-alive)
- Calls `proxyext.BeforeForward(fc)` per attempt to inject account auth
- Calls `s.cacheTTLDetector.RecordRequest(threadID)` per attempt (mirrors stock path)
- Calls `proxyext.OnPreStreamResponse(fc, resp)` before any write to the client
- Stores winning auth in `smmWinningAuth` before `w.WriteHeader` so keepalive
  and forked-agent blocks in `forwardWithAnnotation` use the correct credential
- Sets `fc.BytesFlushed = true` before `w.WriteHeader` (categorical retry gate)
- Forwards the last upstream response on exhaustion (client gets real HTTP status)
- Calls `proxyext.OnPostResponse` on every terminal path (success, transport error,
  exhaustion)

`proxy_forward.go` gate:

```go
if proxyext.IsActive() && s.smmCfg != nil && s.smmCfg.AccountPool.Enabled {
    if smmErr := SMMForwardWithRetry(
        origReq.Context(), threadID, isSubagentFromBody(body),
        w, body, proxyReq, s,
    ); smmErr != nil {
        s.logger.Printf("[req %d] smm forward error: %v", reqIdx, smmErr)
    }
    return  // SMM path owns full response
}
```

The `return` is intentional: the SSE parsing loop, usage tracking, sawtooth,
and fork logic below the gate are deferred to v2. See `V2_GAPS.md`.

---

## Open Questions — ALL RESOLVED (Session 3)

| # | Question | Answer | Source |
|---|----------|--------|--------|
| 1 | Does `proxy_forward.go` have multiple response paths each needing hook call sites? | Two paths exist: `forwardWithAnnotation` (SMM gate added) and `forwardRaw` (intentionally excluded — non-Claude passthrough only; comment added) | Live read |
| 2 | Does `provider_autoconf.go` already parse `~/.claude/` dirs? | **Unread** — deferred; `oauth_store.go` is self-contained and does not call upstream autoconf. Divergence risk is low for v1 (oauth_store reads the same file format). Track in v2. | Deferred |
| 3 | Is `cache_ttl_detect.go` thread-scoped or global? | **Unread** — `RecordRequest(threadID)` is called per-attempt in `SMMForwardWithRetry`, matching the stock path exactly. If the detector is global this is still correct. | Deferred |
| 4 | Does `sawtooth.go` key the frozen prefix on auth-derived state? | **Unread** — sawtooth is not called on the SMM path (v1 scope boundary). No risk in v1. | Deferred |
| 5 | Does `compress_context.go` normalise large stable blocks? | **Unread** — `TransformStaticPayload` is a no-op in v1; `staticplan/` exists at `mode: off`. No wiring added. | Deferred |

---

## Verified-Correct Design Decisions

- **No `X-SMM-Account` header** — account identity stored in `fc.SelectedAccount` only; never written to any outbound header.
- **`BytesFlushed` as categorical gate** — enforced in dispatcher (`hooks.go`), implementation (`extension.go`), and retry loop (`proxy_forward_smm.go`) as defence-in-depth.
- **`SelectedAccount interface{}`** — keeps `types.go` free of the `accountpool` import; avoids import cycle.
- **`smmHTTPClient` never existed** — `s.httpClient` used throughout. Shared transport pool confirmed.
- **`smmWinningAuth` stored before first write** — keepalive and fork auth correct for subsequent same-thread requests.
- **`fail open` on hook error** — `OnPreStreamResponse` error is logged and execution continues (fail-open). Hook failure rate is visible in logs via `[smm]` prefix.
- **`TransformStaticPayload` is a no-op in v1** — `compress_context.go` unread; staticplan deferred.
- **`ResetHooksForTest`** — prevents parallel test races on the process-level singleton.
- **Panic in `OnPostResponse` is logged, not silently discarded.**
- **`forwardRaw` explicitly excluded** — comment added: "SMM hooks are NOT applied here — this path is for non-Claude passthrough only."

---

## v1 Scope Boundary

The following are **deliberately out of scope for v1** and tracked in `V2_GAPS.md`:

- SSE token usage tracking on the SMM path
- `sawtoothTrigger.UpdateAfterResponse` on the SMM path
- `cacheStatusWriter.Update` on the SMM path
- `fireForkedAgents` on the SMM path
- `provider_autoconf.go` divergence audit
- Feature B / `staticplan` wiring
- Per-account TTL scoping

---

## Rollback

All SMM code lives in `internal/proxyext/` and `internal/proxy/proxy_forward_smm.go`.
To disable all SMM features without removing code:

```yaml
smm:
  enabled: false
```

With `enabled: false`, `NewSMMHooks` returns `DefaultHooks()` and
`proxyext.IsActive()` returns `false` — the gate in `proxy_forward.go`
is never entered. No account pool is initialised. No prompt transforms run.
The proxy behaves identically to upstream.

To remove the wiring entirely, revert the single commit that added the
gate block to `proxy_forward.go` (`6ddd8dc`). The `internal/proxyext/`
package and `proxy_forward_smm.go` can remain — they have no effect
until `proxyext.IsActive()` returns true.

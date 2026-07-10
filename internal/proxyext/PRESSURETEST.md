# SMM Pressure-Test Audit — 2026-07-10

This document records every finding from the red-team pressure test of
`feature/smm-proxyext-v1`. Each finding is marked **VERIFIED** (based on
direct file reads) or **INFERRED** (based on architecture reasoning without
reading the file).

Findings are grouped by resolution status.

---

## ✅ Resolved Before This Commit (Already Correct in Code)

### F1: `X-SMM-Account` header sent to Anthropic — VERIFIED RESOLVED

The previous critique predicted this bug. The actual code does not have it.
`extension.go:BeforeForward` stores the selected account in
`fc.SelectedAccount` (an `interface{}` field on `ForwardContext`), never in
an outbound header. Anthropic never receives account identity.

### F2: `ForwardContext` missing `BytesFlushed` and `SelectedAccount` — VERIFIED RESOLVED

`types.go` already contains both fields with full documentation:
- `BytesFlushed bool` — set by proxy_forward.go before OnPreStreamResponse
- `SelectedAccount interface{}` — type-asserted to `accountpool.AccountRef`
  by BeforeForward and OnPreStreamResponse

### F3: `OnPreStreamResponse` reconstructing AccountRef from header — VERIFIED RESOLVED

The current `extension.go:OnPreStreamResponse` type-asserts `fc.SelectedAccount`
directly. No header reconstruction. Full `AccountRef` (Name, CredentialDir,
Priority) is available.

### F4: `BytesFlushed` not checked before retry — VERIFIED RESOLVED

Two layers of defence:
1. `hooks.go:OnPreStreamResponse` dispatcher short-circuits with
   `Retry:false, Reason:"dispatcher_bytes_flushed"` before calling any
   implementation.
2. `extension.go:OnPreStreamResponse` also checks `fc.BytesFlushed` as
   defence-in-depth.

### F5: `selector.go` concurrency — VERIFIED SAFE

`RoundRobinSelector.Select` holds `sync.Mutex` for the full duration.
`MarkResult` acquires only the `StateStore` mutex (not `r.mu`). The
documented precision trade-off (one extra attempt on a cooling account)
is acceptable and documented in selector.go comments.

### F6: `ResetHooksForTest` race safety — VERIFIED CORRECT

`hooks.go` exposes `ResetHooksForTest()` under `hooksMu.Lock()`. Parallel
tests must call `t.Cleanup(proxyext.ResetHooksForTest)` before
`t.Parallel()` to avoid racing on the package-level singleton.

### F7: `noop.go` "byte-identical" claim — VERIFIED (wording corrected)

The spec's wording was misleading. `noop.go` is semantically identical to
the no-extension path — not byte-identical. The dispatcher adds a virtual
call frame and a `sync.RWMutex` read lock per request even on the noop
path. This is negligible in practice (<1µs). The test plan's golden
regression tests verify *output* identity, not byte identity of the wire
format.

---

## ✅ Fixed in This Commit

### F8: `TransformStaticPayload` silently swallowed errors — VERIFIED FIXED

**Previous behaviour:** `_ = h.TransformStaticPayload(fc, prompt)` — error
discarded with no log. A broken staticplan was invisible to operators.

**Fix:** `hooks.go:TransformStaticPayload` now logs `smm_static_plan_fail=true`
with the error string and thread ID when an error is swallowed. The fail-open
contract is preserved (request never dropped). Operators can now distinguish:
- No log line → staticplan ran and was a no-op (healthy)
- `smm_static_plan_noop_reason=...` → staticplan ran, chose not to act
- `smm_static_plan_fail=true err=...` → staticplan ran and errored (silently recovered)

### F9: `IsEnabled()` helper absent — FIXED

Added `IsEnabled() bool` to `hooks.go`. This is the single gate
`proxy_forward.go` should use to enter the SMM code path. It combines
the `activeHooks != nil` and `activeCfg.Enabled` checks under one RLock.
`IsActive()` is retained (deprecated comment added) for any callers that
need hook-presence independently of the config flag.

---

## ⚠️ Inferred Findings — Must Verify Before proxy_forward.go Wiring

These findings could not be confirmed because the relevant upstream files
were not read during the audit. **Do not write a single line of
proxy_forward.go hook wiring until each item below is resolved.**

### I1: Retry loop has no home — INFERRED CRITICAL

`extension.go:OnPreStreamResponse` returns `RetryDecision{Retry: true}`
but nothing in the visible code implements the retry loop itself. The loop
must live in `internal/proxy/proxy_forward.go`.

**Required actions before wiring:**
1. Read `proxy_forward.go` in full.
2. Find the pre-stream boundary (status readable, body not yet flushed).
3. Determine if there are multiple response paths (streaming, non-streaming,
   OpenAI parity) — each may need its own hook call site.
4. Write the retry loop:
   ```go
   // Pseudocode — confirm exact shape against proxy_forward.go structure
   for attempt := 0; attempt <= smmCfg.MaxPreStreamRetries; attempt++ {
       fc.Attempt = attempt
       if err := proxyext.BeforeForward(fc); err != nil {
           return err // all accounts exhausted
       }
       fc.OutboundReq.Body = io.NopCloser(bytes.NewReader(fc.OriginalBody))
       resp, err := httpClient.Do(fc.OutboundReq)
       if err != nil { return err }

       dec, _ := proxyext.OnPreStreamResponse(fc, resp)
       if !dec.Retry {
           break // flush resp to client
       }
       resp.Body.Close()
   }
   ```
5. Ensure `fc.BytesFlushed = true` is set atomically before the first
   `w.Write` or `http.Flusher.Flush()` call.

### I2: `provider_autoconf.go` overlap with `oauth_store.go` — INFERRED

`oauth_store.go` implements `LocalOAuthStore` independently. `provider_autoconf.go`
(15KB) may already parse Claude credential dirs. If so, `oauth_store.go`
should delegate to it rather than maintaining a parallel implementation.
Two credential parsers that can diverge is a maintenance hazard.

**Action:** Read `provider_autoconf.go`. If it exposes a credential-dir
reader, refactor `oauth_store.go` to call it.

### I3: `compress_context.go` may make Feature B redundant — INFERRED

`compress_context.go` (7.6KB) may already normalize or restructure large
prompt blocks — exactly what `staticplan/normalize_text` mode does. Building
a second content transform without auditing this file risks:
- Silent duplicate work on the same bytes
- Two transforms that produce different outputs for the same input
  (cache-breaking)

**Action:** Read `compress_context.go`. If it normalizes stable blocks,
defer or drop Feature B v1. `TransformStaticPayload` remains wired as
a no-op (current state) until this is resolved.

### I4: `cache_ttl_detect.go` scope unknown — INFERRED

If `cache_ttl_detect.go` is global (process-scoped), account rotation is
safe — all accounts share one TTL observation. If it is thread/session-scoped,
rotating accounts on a thread will attribute account B's cache behaviour
to account A's thread state.

**Action:** Read `cache_ttl_detect.go`. If thread-scoped, `OnPostResponse`
must record per-account cache observations.

### I5: `sawtooth.go` key derivation — INFERRED

Sawtooth collapse freezes a prompt prefix to maintain cache stability.
If the frozen-prefix key encodes any auth-derived data, account rotation
would invalidate sawtooth state. If the key is derived purely from prompt
content (expected), rotation is safe.

**Action:** Read `sawtooth.go`. Confirm the freeze key does not include
any credential or session-auth component.

### I6: `proxy_forward.go` has multiple response paths — INFERRED

With 77KB and a sawtooth subsystem + OpenAI parity pipeline, there may be
multiple response-handling branches. Each branch that reads headers before
flushing needs its own `proxyext.OnPreStreamResponse` call site. A single
insertion point may miss the OpenAI parity path or the non-streaming path.

**Action:** Read `proxy_forward.go` fully. Map all response paths before
writing any hook call sites.

---

## 🔴 Explicit Defers (Not Building in v1)

| Item | Reason |
|------|--------|
| Retry loop in proxy_forward.go | Cannot write safely without reading the file |
| TransformStaticPayload wiring | Blocked on compress_context.go audit (I3) |
| Feature B staticplan active mode | Blocked on compress_context.go (I3) |
| experimental_multimodal mode | Explicitly out of scope — not in StaticPlanCfg |
| apply_to_subagents: true | Default false; emits warning log if set true |
| File persistence for account state | v2 only |
| per-account TTL recording | Blocked on cache_ttl_detect.go scope (I4) |

---

## Commit Order (Remaining)

The commit order from the original spec is partially complete. Remaining
steps in correct dependency order:

```
DONE  1: hook scaffolding (hooks.go, noop.go, types.go) — verified complete
DONE  2: extension.go — BeforeForward + OnPreStreamResponse + OnPostResponse
DONE  3: accountpool — state, selector, classify, oauth_store, manager
DONE  4: account pool unit tests (classify_test.go, state_test.go)
DONE  5: proxyext_test.go — hook layer unit tests
THIS  6: pressure-test fixes (hooks.go logging, IsEnabled, this doc)

NEXT  7: READ proxy_forward.go, provider_autoconf.go, sawtooth.go,
            cache_ttl_detect.go, compress_context.go, prompt_cache.go
      8: Wire BeforeForward + retry loop into proxy_forward.go
      9: Wire OnPreStreamResponse into proxy_forward.go (pre-flush boundary)
     10: Decide Feature B go/defer/drop based on compress_context.go
     11: Integration tests
     12: Golden regression tests
     13: Rebase/upgrade playbook + docs
```

---

## Rollback

To disable all SMM features without removing code:

```yaml
# yesmem config
smm:
  enabled: false
```

With `enabled: false`, `NewSMMHooks` returns `DefaultHooks()` (noop).
`IsEnabled()` returns false. The proxy_forward.go retry loop is never
entered. Zero overhead beyond one RLock read per request.

To remove the extension entirely from a build:
1. Remove the `proxyext.Init(...)` call from `proxy.go`.
2. Remove the `proxyext.BeforeForward` / `proxyext.OnPreStreamResponse`
   call sites from `proxy_forward.go` (added in step 8/9 above).
3. The `internal/proxyext/` tree can be left in place — it is never
   imported if the call sites are removed.

---

## Operator Notes

- **Never set `account_pool.apply_to_subagents: true` in v1** — subagent
  interaction with account rotation is unverified.
- **Account names never appear in prompt text** — verified in BeforeForward.
- **Thread/session IDs are preserved across rotation** — `fc.ReqCtx.ThreadID`
  is immutable after construction.
- **All state is in-memory** — a process restart resets all cooldown timers.
  This is intentional in v1.
- **Logs are structured** — filter on `[smm]` prefix. Key fields:
  `smm_account_selected`, `smm_retry_decision`, `smm_static_plan_fail`.
- **No raw tokens in logs** — only account names and thread IDs appear.
  The `Authorization: Bearer` value is never logged.

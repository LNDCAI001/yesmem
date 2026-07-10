# SMM Extension — Decision Log

This file records architectural decisions, pressure-test findings, and
open questions for the `feature/smm-proxyext-v1` branch. Append entries
in reverse-chronological order (newest first).

---

## 2026-07-10 — Red-team cleanup after v0.3-grounded pressure test

### What the pressure test found

The v0.3-grounded spec was pressure-tested against the actual branch.
Three bugs flagged in the red-team report were **already resolved** in
the scaffolding before the test ran:

| Bug (as reported) | Actual status |
|-------------------|---------------|
| `X-SMM-Account` header sent to Anthropic | ❌ Does not exist — account identity is stored in `fc.SelectedAccount` only |
| `ForwardContext` missing `BytesFlushed` | ❌ `BytesFlushed bool` is present in `types.go` with full doc comment |
| `ForwardContext` missing `SelectedAccount` | ❌ `SelectedAccount interface{}` is present in `types.go` with rationale comment |

The spec's "LIKELY TODO" entries (`oauth_store.go`, `retry.go`,
`state.go`, `classify.go`) are all present and contain real
implementations. The spec was out of date relative to the branch.

### What was actually fixed in this cleanup commit

**`hooks.go` — panic recovery in `OnPostResponse` now logs the recovered value.**

Previous code:
```go
if r := recover(); r != nil {
    _ = r  // silent discard
}
```

Fixed code:
```go
if r := recover(); r != nil {
    if l != nil {
        l.Printf("[smm] OnPostResponse panic recovered: %s", fmt.Sprintf("%v", r))
    }
}
```

Rationale: silent discard masks bugs in hook implementations. A panicking
hook should be visible in operator logs so it can be diagnosed and fixed.
The server still does not re-panic — a hook panic must never crash the
server. `dispatchLog` is now passed into `Init()` alongside the hook and
config; the `Init` signature changed from `Init(h Hooks, cfg *SMMConfig)`
to `Init(h Hooks, cfg *SMMConfig, logger *log.Logger)`.

**`SMM_STATUS.md` added.**

A terse operator-facing status file. Records what is built, what is
verified-correct, the one remaining critical TODO (retry loop wiring),
the five upstream files to read before touching `proxy_forward.go`,
and rollback instructions.

### One remaining critical gap

**The retry loop is not wired into `proxy_forward.go`.**

`OnPreStreamResponse` can return `RetryDecision{Retry: true}` but
there is no caller in `proxy_forward.go` that acts on it. Feature A
is structurally complete in `internal/proxyext/` but is a no-op until
this wiring commit is written.

Pre-conditions before writing the wiring commit:
1. Read `internal/proxy/proxy_forward.go` — identify all response paths
   (streaming, non-streaming, OpenAI parity) and confirm how many hook
   call sites are needed.
2. Read `internal/proxy/provider_autoconf.go` — confirm `oauth_store.go`
   does not duplicate upstream credential-dir parsing.
3. Read `internal/proxy/cache_ttl_detect.go` — confirm scoping.
4. Read `internal/proxy/compress_context.go` — make Feature B go/defer/drop
   decision before touching `prompt_rewrite.go`.
5. Read `internal/proxy/sawtooth.go` — confirm frozen-prefix key does not
   encode auth-derived state.

None of these files have been read yet. Do not write the wiring commit
until all five have been inspected.

### Design decisions confirmed by pressure test

- `SelectedAccount interface{}` — correct approach to break the import
  cycle between `proxyext` and `accountpool`. The concrete type is
  `accountpool.AccountRef`; callers assert it at the call site.
- `BytesFlushed` as a categorical gate enforced at two levels
  (dispatcher in `hooks.go` + implementation in `extension.go`) is the
  correct defence-in-depth pattern for the no-retry-after-flush invariant.
- `TransformStaticPayload` as a no-op in `extension.go` for v1 is the
  correct decision until `compress_context.go` is audited.
- `ExperimentalMultimodal` absent from `StaticPlanCfg` — correct; v1
  non-goal. Do not add until compress_context audit is complete.
- `ResetHooksForTest()` — correct pattern for parallel test safety on the
  process-level singleton.

---

## 2026-07-09 — Initial scaffolding committed

Initial `proxyext/` package tree committed on `feature/smm-proxyext-v1`:

- `hooks.go` — Hooks interface, process singleton, four dispatchers
- `noop.go` — DefaultHooks() pure no-op implementation  
- `types.go` — ForwardContext, RetryDecision, ForwardResult, AssembledPrompt
- `extension.go` — SMMConfig, NewSMMHooks, smmHooks implementation
- `accountpool/` — Pool, RoundRobinSelector, AccountState, LocalOAuthStore,
  ShouldRetry, failure classifier
- `staticplan/` — package present, content deferred pending compress_context audit
- `observability/` — package stub
- `proxyext_test.go` — dispatcher unit tests
- `accountpool/state_test.go` — state mutation tests
- `accountpool/classify_test.go` — classifier table tests

Design constraint at this commit: every file in `internal/proxyext/` is
additive. No upstream `internal/proxy/` file has been modified. The
proxy_forward.go wiring is the only remaining change that touches upstream
files.

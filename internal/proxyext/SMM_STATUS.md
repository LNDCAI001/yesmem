# SMM Extension — Branch Status

**Branch:** `feature/smm-proxyext-v1`  
**As of:** 2026-07-10  
**Spec version:** v0.3-grounded (pressure-tested)

---

## What Is Built and Verified

| File | Status | Notes |
|------|--------|-------|
| `proxyext/hooks.go` | ✅ Complete | Dispatcher, singleton, `ResetHooksForTest`, `BytesFlushed` gate |
| `proxyext/noop.go` | ✅ Complete | Four pure no-ops, zero allocations |
| `proxyext/types.go` | ✅ Complete | `ForwardContext` with `BytesFlushed`, `SelectedAccount`, `OriginalBody` |
| `proxyext/extension.go` | ✅ Complete | Auth injection into header only, `SelectedAccount` stored in `fc` not in outbound header |
| `accountpool/types.go` | ✅ Complete | `AccountRef`, `Config`, `TokenResult`, `RequestMeta`, `AccountResult` |
| `accountpool/state.go` | ✅ Complete | Per-account state with `sync.RWMutex`, cooldown logic |
| `accountpool/selector.go` | ✅ Complete | Round-robin with cooldown skip |
| `accountpool/classify.go` | ✅ Complete | Failure classifier table per spec |
| `accountpool/manager.go` | ✅ Complete | `Pool`, `SelectAndGetToken`, `ShouldRetry`, `RecordSuccess` |
| `accountpool/oauth_store.go` | ✅ Complete | `LocalOAuthStore`, reads `~/.claude/` credential dir |
| `accountpool/retry.go` | ✅ Complete | `ShouldRetry` logic, max-retries enforcement |
| `accountpool/state_test.go` | ✅ Present | State mutation and cooldown tests |
| `accountpool/classify_test.go` | ✅ Present | Classifier table tests |
| `proxyext/proxyext_test.go` | ✅ Present | Hook dispatcher tests |

---

## One Critical TODO

**The retry loop is not wired into `proxy_forward.go`.**

All accountpool logic is complete and correct in isolation. But
`OnPreStreamResponse` returning `RetryDecision{Retry: true}` currently has
no caller that acts on it. Feature A is a no-op until this wiring exists.

### What the wiring must do

```
// In proxy_forward.go — pseudocode only
for attempt := 0; attempt <= maxRetries; attempt++ {
    fc.Attempt = attempt
    if err := proxyext.BeforeForward(&fc); err != nil {
        return err // hard exhaustion — surface to client
    }
    fc.OutboundReq.Body = io.NopCloser(bytes.NewReader(fc.OriginalBody))
    resp, err := httpClient.Do(fc.OutboundReq)
    // ... read response headers ...
    dec, _ := proxyext.OnPreStreamResponse(&fc, resp)
    if !dec.Retry {
        break // proceed to stream
    }
    resp.Body.Close() // discard body before retry
}
// fc.BytesFlushed must be set to true before the first Write to client
```

### Before editing `proxy_forward.go`

Read these five upstream files first (none have been read yet):

1. `internal/proxy/proxy_forward.go` — find the exact line where
   `httpClient.Do` is called and where response headers are first readable
   before `w.WriteHeader`. There may be multiple response paths (streaming,
   non-streaming, OpenAI parity) — each needs its own hook call site.
2. `internal/proxy/provider_autoconf.go` — verify `oauth_store.go` does
   not duplicate credential-dir parsing that already exists upstream.
3. `internal/proxy/cache_ttl_detect.go` — determine if thread-scoped or
   global before deciding whether `OnPostResponse` needs per-account TTL.
4. `internal/proxy/compress_context.go` — determine if `staticplan` is
   redundant before wiring `TransformStaticPayload`.
5. `internal/proxy/sawtooth.go` — verify the frozen-prefix key does not
   encode any auth-derived state that account rotation could invalidate.

---

## Verified-Correct Design Decisions

- **No `X-SMM-Account` header** — account identity stored in `fc.SelectedAccount` only.
- **`BytesFlushed` as categorical gate** — enforced in both the dispatcher (`hooks.go`) and the implementation (`extension.go`) as defence-in-depth.
- **`SelectedAccount interface{}`** — keeps `types.go` free of the `accountpool` import; concrete type asserted at call site.
- **`fail open` everywhere except hard exhaustion** — `BeforeForward` logs and returns nil on selection errors; only `IsExhausted` surfaces as an error to the caller.
- **`experimental_multimodal` absent from config struct** — not encodable in v1; cannot be accidentally enabled.
- **`TransformStaticPayload` is a no-op in v1** — `compress_context.go` must be read before staticplan is wired.
- **`ResetHooksForTest`** — prevents parallel test races on the process-level singleton.
- **Panic in `OnPostResponse` is logged, not silently discarded** — operators can see misbehaving hooks.

---

## Open Questions (Must Resolve Before Shipping Feature A)

| # | Question | Risk if wrong |
|---|----------|---------------|
| 1 | Does `proxy_forward.go` have multiple response paths each needing hook call sites? | Missing a path means partial coverage — some requests bypass account pool |
| 2 | Does `provider_autoconf.go` already parse `~/.claude/` dirs? | If yes, `oauth_store.go` may diverge from upstream token refresh |
| 3 | Is `cache_ttl_detect.go` thread-scoped or global? | If thread-scoped, account rotation silently corrupts TTL inference |
| 4 | Does `sawtooth.go` key the frozen prefix on any auth-derived state? | If yes, account rotation invalidates sawtooth freeze |
| 5 | Does `compress_context.go` already normalise large stable blocks? | If yes, Feature B (staticplan) is redundant and should be deferred |

---

## Rollback

All SMM code lives in `internal/proxyext/`. To disable all SMM features
without removing code:

```yaml
smm:
  enabled: false
```

With `enabled: false`, `NewSMMHooks` returns `DefaultHooks()` and the
process-level `activeHooks` is a noop. No account pool is initialised.
No prompt transforms run. The proxy behaves identically to upstream.

To remove the wiring from `proxy_forward.go` cleanly, revert only the
two call sites added in the wiring commit (not yet written). The
`internal/proxyext/` package itself can remain — it has no effect
until `Init()` is called.

# SMM Fork — Operator Handoff

**Repo:** https://github.com/LNDCAI001/yesmem  
**Branch:** `feature/smm-proxyext-v1`  
**Upstream:** https://github.com/carsteneu/yesmem  
**Status as of:** 2026-07-10 (Session 3 complete)

---

## What This Branch Does

Adds a **Subscription Multi-Model (SMM)** account-pool layer to the yesmem
proxy. When enabled, instead of forwarding requests with the inbound client
credential, the proxy selects an account from a locally-configured pool of
Anthropic subscription accounts, injects that account's OAuth Bearer token,
and retries with a different account if the response indicates quota
exhaustion or auth failure — all before the first byte reaches the client.

When disabled (`smm.enabled: false`), the proxy is **byte-identical to
upstream**. The gate is a single `if proxyext.IsActive()` check in
`proxy_forward.go`.

---

## Build

```bash
git checkout feature/smm-proxyext-v1
go build ./...
go test -race ./internal/proxyext/...
```

Both must pass clean before deploying.

---

## Configuration

Add to your `config.yaml` (or equivalent config source):

```yaml
smm:
  enabled: true
  account_pool:
    enabled: true
    max_pre_stream_retries: 3   # attempts before surfacing error to client
    accounts:
      - credential_dir: ~/.claude/account-1   # directory containing OAuth tokens
      - credential_dir: ~/.claude/account-2
      - credential_dir: ~/.claude/account-3
```

The `credential_dir` for each account must contain the OAuth token files
in the same format that the Claude desktop app writes to `~/.claude/`.
`oauth_store.go` reads these files directly — no manual token entry needed.

To disable without removing config:

```yaml
smm:
  enabled: false
```

---

## Smoke Test

Verify the SMM-disabled path is byte-identical to upstream before any
live account testing:

```bash
# 1. SMM disabled — record response
SMM_ENABLED=false go run . &
curl -s localhost:PORT/v1/messages \
  -H "x-api-key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-opus-4-5","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}' \
  > out_smm_disabled.json

# 2. Upstream build — record response
git stash && go run . &
curl -s localhost:PORT/v1/messages \
  -H "x-api-key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-opus-4-5","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}' \
  > out_upstream.json

# 3. Diff must be empty
diff out_smm_disabled.json out_upstream.json
```

---

## Architecture

```
client request
    │
    ▼
proxy_forward.go — forwardWithAnnotation
    │
    ├─ proxyext.IsActive()? ──YES──▶ proxy_forward_smm.go
    │                                    │
    │                          ┌─────────┴──────────┐
    │                          │  for each attempt:  │
    │                          │  BeforeForward(fc)  │ ← injects account auth
    │                          │  s.httpClient.Do()  │ ← shared transport
    │                          │  OnPreStreamResponse│ ← retry decision
    │                          │  [retry? close+loop]│
    │                          │  smmWinningAuth     │ ← store winning auth
    │                          │  w.WriteHeader      │
    │                          │  io.Copy → client   │
    │                          │  OnPostResponse     │ ← account state update
    │                          └─────────────────────┘
    │
    └─ NO ──▶ stock path (unchanged from upstream)
                  s.httpClient.Do()
                  SSE parse loop
                  usage tracking
                  sawtooth
                  forked agents
```

---

## Key Files

| File | Role |
|------|------|
| `internal/proxyext/types.go` | `ForwardContext`, `SMMConfig`, `RetryDecision` |
| `internal/proxyext/hooks.go` | Dispatcher singleton, `BeforeForward`, `OnPreStreamResponse`, `OnPostResponse` |
| `internal/proxyext/extension.go` | Auth injection, account selection, retry classification |
| `internal/proxyext/noop.go` | No-op implementation (used when SMM disabled) |
| `internal/proxyext/accountpool/` | Account pool: state, selector, classifier, oauth store, retry logic |
| `internal/proxy/proxy_forward_smm.go` | Retry loop wiring |
| `internal/proxy/proxy_forward.go` | Gate: `if proxyext.IsActive()` |

---

## Security Invariants (Non-Negotiable)

1. **No token strings in any log line.** `oauth_store.go` must redact
   before logging. The `[smm]`-prefixed log lines in `proxy_forward_smm.go`
   log only error strings, never auth values.

2. **`x-api-key` is deleted from outbound requests** in both canonical
   (`X-Api-Key`) and raw (`x-api-key`) form in `extension.go`. Do not
   remove these deletions.

3. **`SelectedAccount` never appears in any outbound header name or value.**
   Account identity is stored in `fc.SelectedAccount` (in-process only).
   The only auth-related outbound header is `Authorization: Bearer <token>`,
   which carries the OAuth token — not any account identifier.

4. **No middleware may re-add the original client `Authorization` header**
   between `SMMForwardWithRetry` and `s.httpClient.Do`. The call chain is
   direct: `SMMForwardWithRetry` → `cloneForAttempt` → `BeforeForward` →
   `s.httpClient.Do`. There is no middleware in this path.

---

## Known v1 Gaps (Not Bugs)

The following features from the stock path do not run on the SMM path in v1.
See `internal/proxyext/V2_GAPS.md` for details and v2 implementation plan.

- SSE token usage tracking (daemon `_track_usage` call)
- `sawtoothTrigger.UpdateAfterResponse`
- `cacheStatusWriter.Update`
- `fireForkedAgents`
- Feature B / staticplan (`TransformStaticPayload` is a no-op)

---

## Rollback

To disable at runtime: set `smm.enabled: false` in config and restart.
No code changes needed.

To revert the wiring commit from the codebase:

```bash
git revert 6ddd8dc  # proxy_forward.go gate commit
```

`internal/proxyext/` and `proxy_forward_smm.go` can remain in the tree —
they have zero effect until `proxyext.IsActive()` returns true.

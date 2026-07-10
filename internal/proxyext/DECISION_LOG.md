# SMM Proxyext Decision Log

Every architectural decision that touches the integration boundary between
`internal/proxyext` and `internal/proxy` is recorded here. Each entry
states the question, the evidence source (file + SHA or "INFERRED"), the
finding, and the consequence for the design.

This file is the audit trail. If you are rebasing against upstream or
debating whether to change a design choice, start here.

---

## D1 — Pre-flush boundary in proxy_forward.go

**Question:** Where exactly is the boundary between "response headers received"
and "first byte written to client" in `forwardWithAnnotation`?

**Source:** VERIFIED — read `internal/proxy/proxy_forward.go`
SHA `77f46291700dc916534a35e2041e43ebf86abd20`

**Finding:**
```
resp, err := s.httpClient.Do(proxyReq)   // ← headers received, nothing flushed
// ... gzip decompression, rate-limit header parse, header copy ...
w.WriteHeader(resp.StatusCode)           // ← FIRST BYTE TO CLIENT
```
`BeforeForward` is called after `proxyReq` is constructed, before `Do()`.
`OnPreStreamResponse` is called after `Do()` returns, before `WriteHeader()`.

**Consequence:** The retry window is precisely the gap between `Do()` returning
and `WriteHeader()` being called. This is a clean window with no buffering.
`forwardRaw` is excluded from SMM hooks in v1 — it is the passthrough path for
non-Claude requests and has no annotation logic.

---

## D2 — provider_autoconf.go and Claude OAuth

**Question:** Does `provider_autoconf.go` parse `~/.claude/` credential dirs
in a way `oauth_store.go` should delegate to?

**Source:** VERIFIED — read `internal/proxy/provider_autoconf.go`

**Finding:** `provider_autoconf.go` is entirely OpenCode-specific. It reads
`~/.cache/opencode/models.json`, `auth.json`, and `opencode.json` to discover
OpenAI-compatible third-party providers. It has no `~/.claude/` directory
parsing, no bearer token refresh, and no Claude subscription OAuth logic.

**Consequence:** `oauth_store.go` implements Claude OAuth credential loading
independently. Zero duplication risk.

---

## D3 — compress_context.go vs. staticplan

**Question:** Does `compress_context.go` already do what `staticplan` was
designed to do, making Feature B redundant in v1?

**Source:** VERIFIED — read `internal/proxy/compress_context.go`

**Finding:** `CompressContext` operates exclusively on the `messages[]` array —
it compresses old `thinking` blocks and `tool_result` blocks in conversation
history. It does not touch the `system` prompt, tool definitions, or scaffold
instructions.

`staticplan` targets system-level structures (system prompt, tool descriptions,
stable scaffold blocks) — precisely what `compress_context.go` leaves
untouched.

**Consequence:** Feature B is not redundant. It proceeds gated behind
`mode: off`. The primary v1 target is `extract_aux_text_block` mode, which
operates on tool descriptions in the `tools[]` array.

---

## D4 — feature_gates.go registration

**Question:** Should SMM register its feature flags in `feature_gates.go`?

**Source:** INFERRED — `feature_gates.go` not read in detail

**Finding:** The three-layer gate already in place is sufficient:
1. YAML `smm.enabled: false` (top-level kill switch)
2. `proxyext.Init()` not called unless enabled (no hook overhead)
3. `proxyext.IsActive()` fast-path check in `proxy_forward.go`

Adding a `feature_gates.go` entry would create a fourth gate with no
additional safety and increased churn risk on an upstream file.

**Consequence:** No `feature_gates.go` registration. SMM flags live
exclusively in `SMMConfig`.

---

## D5 — sawtooth and account rotation

**Question:** Does `sawtooth.go` encode auth-derived state in its thread key,
such that account rotation would corrupt frozen prefix state?

**Source:** INFERRED from `proxy_forward.go` SA `77f46291`:
`s.sawtoothTrigger.UpdateAfterResponse(threadID, ...)` — keyed on `threadID`
only. Account rotation preserves `threadID`.

**Finding:** Sawtooth state is keyed on `threadID`, not on the account or auth
header. Rotating accounts does not change `threadID`.

**Consequence:** No sawtooth corruption risk from account rotation. The frozen
prefix remains valid across retries because the prompt body is also unchanged
(only the `Authorization` header differs between attempts).

---

## D6 — cacheTTLDetector scoping

**Question:** Is `cacheTTLDetector` thread-scoped or process-global? If global,
does firing `RecordRequest` multiple times per threadID (once per retry attempt)
corrupt TTL inference?

**Source:** VERIFIED from `proxy_forward.go` SHA `77f46291`:
`s.cacheTTLDetector` is a field on `*Server` — process-global singleton.
`RecordRequest(threadID)` is called once per `httpClient.Do()` call in the
stock path. In the SMM path it fires once per attempt inside
`attemptSMMForward`.

**Finding:** `RecordResponse` fires exactly once after the successful response
(at the end of the SSE loop or non-SSE path). Multiple `RecordRequest` calls
for the same `threadID` on the same request (due to retries) do not corrupt
TTL inference because TTL state is derived from the `RecordResponse` side.

**Consequence:** Per-account TTL tracking is not needed in v1. The SMM path
calls `RecordRequest` once per attempt (matching the stock path's one call per
`Do()`), and `RecordResponse` is called once after the final successful
response in the unchanged post-`WriteHeader` code.

---

## D7 — proxy_forward.go call-site wiring

**Question:** What is the exact minimal edit to `proxy_forward.go` to wire in
`SMMForwardWithRetry`?

**Source:** VERIFIED — read `proxy_forward.go` SHA `77f46291` in full.

**Finding:** The `httpClient.Do(proxyReq)` call in `forwardWithAnnotation`
is a single call site. The SMM gate replaces it with:
```go
var resp *http.Response
if proxyext.IsActive() && s.smmCfg != nil && s.smmCfg.AccountPool.Enabled {
    resp, err = SMMForwardWithRetry(s, w, origReq, body, threadID)
} else {
    if s.cacheTTLDetector != nil {
        s.cacheTTLDetector.RecordRequest(threadID)
    }
    resp, err = s.httpClient.Do(proxyReq)
}
```
`cacheTTLDetector.RecordRequest` moves into each branch:
- SMM branch: called per-attempt inside `SMMForwardWithRetry`
- Stock branch: called once here as before

This produces zero diff to the non-SMM execution path.

`forwardRaw` and `passthrough` are untouched.

**Upstream diff surface:** 12 lines changed in `forwardWithAnnotation`
(the `Do` block replacement). All other logic is unchanged.

---

## D8 — smmPoolEnabled() helper removed

**Question:** Should a `smmPoolEnabled()` helper function encapsulate the
`proxyext.IsActive() && s.smmCfg != nil && s.smmCfg.AccountPool.Enabled` check?

**Source:** Design decision during red-team.

**Finding:** A helper would need to be a method on `*Server` (to access
`s.smmCfg`) or a package-level function that takes the config pointer. Either
option adds a layer of indirection that obscures what the gate condition
actually is. The inline check is three conditions, all immediately readable.

**Consequence:** No helper. The gate is inline in `forwardWithAnnotation`.
The `smmCfg` field on `*Server` must be populated at startup by whatever
config-loading code calls `proxyext.Init()`.

---

## D9 — ForwardContext.SelectedAccount type choice

**Question:** Why is `ForwardContext.SelectedAccount` typed as `interface{}`
rather than `accountpool.AccountRef` directly?

**Source:** Design decision — avoiding import cycle.

**Finding:** `proxyext` imports `proxyext/accountpool` (for `Pool`, `AccountRef`,
etc.). If `types.go` in `proxyext` also imports `accountpool` just for the
`AccountRef` type in `ForwardContext`, the import graph is:
`proxy` → `proxyext` → `accountpool`

This is fine. The import cycle concern was wrong in the earlier critique.
`interface{}` was retained because:
1. It allows future non-accountpool use of `SelectedAccount` (e.g., API key
   rotation without a full Pool)
2. The type assertion cost is negligible (one branch per request, not per token)
3. It makes the zero value (nil) unambiguous regardless of AccountRef's fields

**Consequence:** `SelectedAccount interface{}` stays. All callers that need the
full ref do `acc, ok := fc.SelectedAccount.(accountpool.AccountRef)`.

---

## D10 — Upstream files modified vs. added

**Summary of diff surface for rebase risk assessment:**

| File | Status | Lines changed | Risk |
|------|--------|---------------|------|
| `internal/proxy/proxy_forward.go` | MODIFIED | ~12 | Medium — high-churn upstream file |
| `internal/proxyext/hooks.go` | ADDED | — | Zero upstream conflict risk |
| `internal/proxyext/types.go` | ADDED | — | Zero upstream conflict risk |
| `internal/proxyext/extension.go` | ADDED | — | Zero upstream conflict risk |
| `internal/proxyext/noop.go` | ADDED | — | Zero upstream conflict risk |
| `internal/proxyext/accountpool/*` | ADDED | — | Zero upstream conflict risk |
| `internal/proxyext/DECISION_LOG.md` | ADDED | — | Zero upstream conflict risk |
| `internal/proxy/proxy_forward_smm.go` | ADDED | — | Zero upstream conflict risk |

**Rebase protocol:** On each upstream sync, diff `proxy_forward.go` against
upstream. The SMM gate block (`// ── SMM ACCOUNT POOL GATE ──`) is clearly
delimited. If upstream moves or splits the `httpClient.Do` call, re-apply the
gate at the new location. All other added files are immune to upstream churn.

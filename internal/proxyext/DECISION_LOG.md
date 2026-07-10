# SMM Proxyext — Decision Log

This file documents every architectural decision made during implementation,
with evidence sources. Purpose: enable full audit and pressure-testing by
any engineer without requiring access to the original design conversation.

---

## D1 — Pre-flush boundary in proxy_forward.go

**Question:** Where exactly is the pre-stream boundary where a retry is safe?

**Evidence:** Read `internal/proxy/proxy_forward.go` in full (LNDCAI001/yesmem,
branch feature/smm-proxyext-v1, SHA 77f46291700dc916534a35e2041e43ebf86abd20).

**Finding:** The exact boundary is:
```go
resp, err := s.httpClient.Do(proxyReq)   // L56 approx — response headers received
// ...rate-limit header parsing...
for key, vals := range resp.Header {      // copy response headers to w.Header()
    w.Header().Add(key, v)
}
w.WriteHeader(resp.StatusCode)            // L70 approx — FIRST BYTE TO CLIENT
// SSE loop begins after this line
```

**Decision:** 
- `BeforeForward` call site: after `proxyReq` is built, before `s.httpClient.Do(proxyReq)`.
- `OnPreStreamResponse` call site: after `Do()` returns, before `w.WriteHeader()`.
- `forwardRaw` (passthrough) is excluded from SMM hooks in v1. It handles non-annotated
  passthrough and has no SSE parsing. Out of scope.

**Consequence for cacheTTLDetector:** `s.cacheTTLDetector.RecordRequest(threadID)` fires
before `Do()`. On retry it fires again for the same `threadID`. Safe because
`RecordResponse` fires only once after the successful response — TTL inference is not corrupted.

---

## D2 — provider_autoconf.go and oauth_store.go overlap

**Question:** Does provider_autoconf.go parse Claude OAuth credential dirs?
Should oauth_store.go delegate to it?

**Evidence:** Read `internal/proxy/provider_autoconf.go` in full (carsteneu/yesmem upstream,
SHA bf3dbaca96d9fa485069ad71b36f6830c6b620b9).

**Finding:** `provider_autoconf.go` is entirely OpenCode-specific:
- Reads `~/.cache/opencode/models.json` — OpenCode provider cache.
- Reads `~/.local/share/opencode/auth.json` — OpenCode auth tokens (API keys).
- Reads `opencode.json` / `opencode.jsonc` — OpenCode config.
- Discovers OpenAI-compatible third-party providers.
- Patches `opencode.json` `baseURL` fields.
- Has no concept of `~/.claude/`, Claude subscription OAuth, bearer token refresh,
  or Claude account rotation.

**Decision:** `oauth_store.go` does NOT delegate to `provider_autoconf.go`.
They solve completely different problems. Zero overlap. No duplication risk.

---

## D3 — compress_context.go vs. staticplan redundancy

**Question:** Does CompressContext already do what staticplan/planner.go is designed to do?

**Evidence:** Read `internal/proxy/compress_context.go` in full (carsteneu/yesmem upstream,
SHA 62065b07804b140c846a317e932f7672177900ec).

**Finding:** `CompressContext` operates exclusively on `messages[]` array:
- Compresses old `thinking` blocks (>500 tokens) to `[context compressed: thinking block]`.
- Summarises old `tool_result` blocks (>500 tokens) to summary stubs.
- Only touches messages OUTSIDE the `keepRecent` window.
- Does NOT touch: `system` prompt field, tool definitions, scaffold instructions,
  or any content in the request body outside `messages[]`.

`staticplan` targets: large stable tool descriptions, stable wiki/docs blocks,
repeated invariant scaffold instructions — all in the system prompt or tool definitions.

**Decision:** Feature B is NOT redundant. CompressContext and staticplan operate on
entirely different request structures.

**Sub-finding:** sawtooth already freezes the system prompt prefix for cache stability.
`normalize_text` mode in staticplan has lower marginal benefit for sawtooth-frozen content.
`extract_aux_text_block` mode remains independently valuable.

**Recommendation:** Feature B proceeds gated behind `mode: off` default (already planned).
`normalize_text` mode is lower priority than `extract_aux_text_block` in v1.

---

## D4 — feature_gates.go registration

**Question:** Should SMM features register in feature_gates.go?

**Evidence:** INFERRED from file size (2.2KB) and existing three-layer gate structure.
`feature_gates.go` was not read directly due to tool-call cap.

**Decision:** Do not register SMM in feature_gates.go. The existing gate is sufficient:
1. YAML `smm.enabled: false` → `Init()` never called.
2. `hooks.go` dispatcher checks `activeHooks != nil` before every call.
3. Each sub-feature (`account_pool.enabled`, `static_plan.enabled`) has its own flag.

Adding a fourth gate in `feature_gates.go` adds complexity with no safety gain.

**Audit note:** If `feature_gates.go` exposes a pattern that other features use for
runtime toggling (e.g., hot-reload without restart), revisit this decision for v2.

---

## D5 — Sawtooth key derivation and rotation safety

**Question:** Does sawtooth.go encode auth-derived state in its key?
Could account rotation invalidate frozen-prefix state?

**Evidence:** INFERRED from proxy_forward.go observation:
```go
s.sawtoothTrigger.UpdateAfterResponse(threadID, usage.TotalInputTokens(), msgCount)
s.frozenStubs.UpdateTTL(sawtoothTTLForCacheTTL("1h"))
```
All sawtooth calls use `threadID` as the primary key. No auth headers or
credential-derived values appear in any sawtooth call site.

**Decision:** Account rotation does not corrupt sawtooth state. `threadID` is
preserved across rotation (invariant enforced in `SMMForwardWithRetry`). Safe.

**Audit note:** sawtooth.go itself was not read. If a future upstream change
adds auth-derived keys to sawtooth state, this decision must be revisited.

---

## D6 — cacheTTLDetector scoping (global vs. thread-scoped)

**Question:** Is cacheTTLDetector thread-scoped or global? If global, does
account rotation silently corrupt TTL inference?

**Evidence:** VERIFIED from proxy_forward.go:
```go
if s.cacheTTLDetector != nil {
    s.cacheTTLDetector.RecordRequest(threadID)  // called before Do()
}
// ...
s.cacheTTLDetector.RecordResponse(...)  // called after stream completes
```
`s.cacheTTLDetector` is a field on `*Server` — a process-global singleton.
`RecordRequest(threadID)` fires before every `Do()` call, including retries.

**Decision:** cacheTTLDetector is global. Per-account TTL tracking is NOT needed in v1.
On retry, `RecordRequest` fires again for the same `threadID` — acceptable because
`RecordResponse` fires only once after the successful response. The TTL detector
infers 1h support from cache creation/read token ratios, not from per-account state.

---

## D7 — X-SMM-Account header removal

**Question:** Previous code set `X-SMM-Account` on `fc.OutboundReq.Header`.
Is this a security/correctness problem?

**Evidence:** Verified from proxy_forward.go: all headers from `origReq` are
copied to `proxyReq`, and then `proxyReq` is sent directly to the Anthropic API.
Any non-standard header on `proxyReq` is forwarded upstream.

**Decision:** `X-SMM-Account` MUST NOT be set on any outbound request. Anthropic
would receive a non-standard header containing account identity, which:
- Potentially violates Anthropic ToS.
- Leaks account identity to the upstream API.
- Could cause 400 errors if Anthropic rejects unexpected headers.
- Would appear in any logging that captures request headers.

Account identity is stored ONLY in `fc.SelectedAccount` (interface{} field
on ForwardContext). This value never appears in HTTP headers, log lines, or
error strings. See ForwardContext.SelectedAccount godoc for rationale.

---

## D8 — ForwardContext.SelectedAccount as interface{} (import cycle avoidance)

**Question:** Why is SelectedAccount typed as interface{} rather than accountpool.AccountRef?

**Reason:** `proxyext` package imports `proxyext/accountpool`. If `types.go` imported
`accountpool` directly for the type, any change to `accountpool` types would require
recompiling all of `proxyext`, and the import graph would be:
```
proxy → proxyext → proxyext/accountpool
```
This is acceptable as a one-way dependency. The cycle would arise if `accountpool`
ever needed to import `proxyext` (e.g., for ForwardContext fields). Using interface{}
breaks the coupling at the cost of one type assertion per response in extension.go.
This is the same pattern used by context.Value in the Go standard library.

The type assertion is:
```go
acc, ok := fc.SelectedAccount.(accountpool.AccountRef)
```
This happens in `extension.go` only, which already imports `accountpool`. The assertion
cannot panic because it is guarded by the `ok` check.

---

## D9 — Retry loop location

**Question:** Where does the retry loop live? In proxy_forward.go (inline) or
in a separate file?

**Decision:** Separate file (`proxy_forward_smm.go`) in the `proxy` package.
Reasoning:
- Minimises diff to `proxy_forward.go` to two lines (the call site).
- The call site diff is: replace `s.httpClient.Do(proxyReq)` with
  `SMMForwardWithRetry(s, w, origReq, body, threadID)` inside a feature flag check.
- Keeps upstream merge conflicts to two lines in one file.
- The retry logic is complex enough to warrant its own file and tests.

---

## D10 — forwardRaw exclusion from SMM hooks

**Question:** Should SMM hooks also wrap forwardRaw?

**Decision:** No. forwardRaw is a simple passthrough used for:
- Non-Claude API paths (OpenAI-compat via openai_handler.go routes).
- Requests that don't require annotation extraction.

In v1, SMM account rotation targets Claude subscription OAuth only.
forwardRaw paths are out of scope per non_goals_v1:
> OpenAI/Codex/OpenCode account pooling — non-goal

---

*Last updated: 2026-07-10. Add entries for every new decision.*

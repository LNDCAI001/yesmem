# SMM v2 — Deferred Feature Gaps

This file tracks post-stream features that are **intentionally out of scope
for v1** (`feature/smm-proxyext-v1`). Each item has a clear reason for
deferral and a concrete description of what v2 must add.

---

## Why These Are Deferred

`SMMForwardWithRetry` in `proxy_forward_smm.go` uses `io.Copy` to stream
the upstream response directly to the client and then returns. The SSE
parsing loop in `forwardWithAnnotation` that handles usage tracking,
sawtooth, cache status writes, and forked-agent dispatch is never reached
on the SMM path.

This is a **clean, intentional v1 scope boundary** — not a bug. The SMM
path is correct and safe. These features simply need their own call sites
inside `SMMForwardWithRetry` or a shared post-stream hook.

---

## Gap 1 — Token Usage Tracking

**What is missing:** On the stock path, `UsageTracker` parses SSE events
and calls `s.queryDaemon("_track_usage", ...)` with real input/output token
counts. This never runs on the SMM path.

**Impact:** The daemon's agent budget tracking is blind to SMM requests.

**v2 fix:** Either pipe the SMM stream through the existing `UsageTracker`
(wrap `io.Copy` with a tee to the SSE parser), or add a lightweight
post-response usage hook to `proxyext.Extension`.

---

## Gap 2 — Sawtooth Trigger

**What is missing:** `s.sawtoothTrigger.UpdateAfterResponse(threadID,
inputTokens, msgCount)` never fires on the SMM path.

**Impact:** Sawtooth context-compression decisions are based on stale token
counts for SMM threads.

**v2 fix:** Requires Gap 1 to be resolved first (need real token counts).
Once `UsageTracker` is wired into the SMM path, feed its output to
`sawtoothTrigger` identically to the stock path.

**Prerequisite:** Read `sawtooth.go` to confirm the frozen-prefix key does
not encode any auth-derived state — if it does, account rotation across
SMM retries could invalidate the freeze.

---

## Gap 3 — Cache Status Writer

**What is missing:** `s.cacheStatusWriter.Update(...)` and
`s.cacheStatusWriter.UpdateThresholdForThread(...)` never fire on the
SMM path.

**Impact:** Cache status display (e.g., the `cache_status.json` written
to `DataDir`) is stale for SMM threads.

**v2 fix:** Same dependency as Gap 1. Feed `UsageTracker` output to
`cacheStatusWriter` after streaming completes.

---

## Gap 4 — Forked Agent Dispatch

**What is missing:** `s.fireForkedAgents(ForkContext{...})` never fires
on the SMM path. The `smmWinningAuth` store in `proxy_forward_smm.go`
correctly provides the per-thread winning credential for *when* forks
are eventually wired — the auth infrastructure is already in place.

**Impact:** Reflection calls and forked agent computations do not run
for SMM requests.

**v2 fix:** Requires Gap 1 (need `usage.Complete` and `usage.CacheReadInputTokens`).
Once usage is tracked, the `fireForkedAgents` call can be lifted into a
post-stream closure inside `SMMForwardWithRetry`, using
`smmWinningAuth.Load(threadID)` to override the fork auth header (the
stock-path `forkAuthHeader` override block in `forwardWithAnnotation` is
already the correct pattern to copy).

---

## Gap 5 — Feature B / staticplan

**What is missing:** `proxyext.TransformStaticPayload` is a no-op in v1.
`staticplan/` exists in the tree at `mode: off`.

**Impact:** None in v1. Large stable prompt blocks are not pre-compressed
for prompt-cache optimisation.

**v2 prerequisite:** Read `compress_context.go` to determine whether it
already normalises large stable blocks. If it does, Feature B is redundant
and should be permanently closed. If it only compresses/truncates by token
count (no normalisation), Feature B is still in scope but requires
verifying `prompt_cache.go` block ordering before wiring
`TransformStaticPayload`.

---

## Gap 6 — provider_autoconf.go Divergence Audit

**What is missing:** `accountpool/oauth_store.go` reads `~/.claude/`
credential directories independently. `provider_autoconf.go` may contain
overlapping credential-parsing logic.

**Impact:** If both parse the same files with different logic, a token
refresh in `oauth_store.go` could diverge from upstream's view of token
validity over time.

**v2 fix:** Read `provider_autoconf.go`. If it parses the same credential
dirs, refactor `oauth_store.go` to delegate to the upstream parser rather
than duplicating it.

---

## Gap 7 — Per-Account TTL Scoping

**What is missing:** `cache_ttl_detect.go` scoping is unverified.
If the detector is thread-scoped (not process-global), account rotation
across SMM retries could corrupt TTL inference for the active thread.

**Impact:** Unknown until `cache_ttl_detect.go` is read.

**v2 fix:** Read `cache_ttl_detect.go`. If thread-scoped, the TTL
detector state may need to be preserved across retries by pinning the
detector instance to the winning account rather than the thread.

---

## Priority Order for v2

1. **Gap 1** (usage tracking) — blocks Gaps 2, 3, 4
2. **Gap 6** (autoconf divergence) — independent, low risk but should be
   audited before any oauth_store changes
3. **Gap 7** (TTL scoping) — read `cache_ttl_detect.go` first
4. **Gap 2 + 3 + 4** (sawtooth, cache status, forks) — after Gap 1
5. **Gap 5** (staticplan) — last; requires reading `compress_context.go`
   before any decision

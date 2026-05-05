# cap_store Analysis — Examples

Concrete end-to-end walkthroughs. See `cap-store-analysis.md` for the API reference.

## 1. Reddit Thread Analysis (the minimum viable loop)

Capture → collect → summarize in chat → persist.

```js
// 1. Fetch & persist a reddit thread
await reddit_fetch({url: 'https://reddit.com/r/LocalLLaMA/comments/abc123/'})
// → stores rows in cap_reddit_fetch__{posts, comments, links}

// 2. Pull all comments for that thread into chat
const res = await cap_collect({
  cap: 'reddit_fetch',
  table: 'comments',
  where: 'post_permalink = ?',
  args: ['https://reddit.com/r/LocalLLaMA/comments/abc123/'],
  instruction: 'Welche Hauptkritikpunkte gibt es?'
})
// res.rows is available in this Claude's conversation.
// You (the conversation Claude) read the rows and write the summary below.

// 3. Persist your in-chat analysis
await cap_save_analysis({
  cap: 'reddit_fetch',
  source_table: 'comments',
  filter_where: 'post_permalink = ?',
  filter_args: ['https://reddit.com/r/LocalLLaMA/comments/abc123/'],
  instruction: 'Welche Hauptkritikpunkte gibt es?',
  summary: 'Drei Cluster: (a) API-Kosten bei großen Kontexten, (b) methodische Zweifel an Nutzen, (c) Lob für Open-Source-Referenz-Implementierungen.',
  row_count: res.count,
  tags: 'llm-wiki,karpathy,kritik'
})
// → { id: 1, cap: 'reddit_fetch', table: 'analyses', ... }
```

## 2. Topic Search Across Captured Knowledge

Find every analysis that touched a specific topic, regardless of when or which source.

```js
await cap_search({
  cap: 'reddit_fetch',
  table: 'analyses',
  where: "instruction LIKE '%API-Kosten%' OR summary LIKE '%API-Kosten%'",
  all: true
})
// → { rows: [...], count: 7, total: 7 }
```

With multiple source caps (e.g. you also have `hn_fetch`, `arxiv_fetch`): run the same query per cap and merge in-chat. Cap_search doesn't cross capabilities by design — each capability is a namespace.

## 3. Author Aggregation

All comments by a specific redditor across all captured threads:

```js
const res = await cap_collect({
  cap: 'reddit_fetch',
  table: 'comments',
  where: 'author = ?',
  args: ['karpathy'],
  max_rows: 1000
})
// res.rows now contains all karpathy comments — analyze in chat
```

Then save the meta-analysis:

```js
await cap_save_analysis({
  cap: 'reddit_fetch',
  source_table: 'comments',
  filter_where: 'author = ?',
  filter_args: ['karpathy'],
  instruction: 'Was sind wiederkehrende Themen in karpathys Reddit-Kommentaren?',
  summary: 'Fokus auf LLM-Pedagogik, spektrale Analyse von Attention, Kritik an benchmark-farming. Ton: technisch-präzise, selten wertend.',
  row_count: res.count,
  tags: 'author-profile,karpathy'
})
```

## 4. Tag-Based Recall

Two months after you tagged analyses with `llm-wiki,karpathy`, find them all:

```js
await cap_search({
  cap: 'reddit_fetch',
  table: 'analyses',
  where: "tags LIKE '%llm-wiki%'",
  all: true
})
```

AND-match on multiple tags:

```js
await cap_search({
  cap: 'reddit_fetch',
  table: 'analyses',
  where: "tags LIKE '%karpathy%' AND tags LIKE '%kritik%'"
})
```

Tag convention: comma-separated, lowercase, kebab-case. No leading/trailing comma needed — LIKE `%tag%` handles it.

## 5. Recursive Meta-Analysis (the long-horizon payoff)

You've accumulated 80 analyses over 3 months. Now ask: what patterns emerge across all LLM-Wiki-related discussions?

```js
const meta = await cap_collect({
  cap: 'reddit_fetch',
  table: 'analyses',                                        // ← the analyses table!
  where: "tags LIKE '%llm-wiki%' AND created_at >= ?",
  args: ['2026-02-01'],
  instruction: 'Finde wiederkehrende Themen und Positionen über alle LLM-Wiki-Analysen',
  mode: 'haiku',                                            // delegate for cost control
  max_tokens: 3000
})
// → { mode: 'haiku', summary: '...', rows_used: 12, total_in_db: 12, ... }

// Save the meta-result too — it's just another analysis
await cap_save_analysis({
  cap: 'reddit_fetch',
  source_table: 'analyses',                                 // ← meta-analysis of analyses
  filter_where: "tags LIKE '%llm-wiki%' AND created_at >= ?",
  filter_args: ['2026-02-01'],
  instruction: meta.source.instruction,
  summary: meta.summary,
  row_count: meta.rows_used,
  model: meta.model,
  tags: 'meta,llm-wiki,Q1-2026'
})
```

This is the compounding benefit: every analysis you save today becomes a searchable row for tomorrow's meta-analysis.

## 6. Headless Haiku (for cron / automation)

Scenario: daily cron job summarizes the last 24h of captured content without a human Claude in the loop.

Prereq: `ANTHROPIC_API_KEY` is set in the environment where the cron runs (or in `~/.claude/yesmem/config.yaml` under `api_key:`).

```js
// In a scheduled script (e.g. via CronCreate or external cron):
await cap_collect({
  cap: 'reddit_fetch',
  table: 'comments',
  where: "fetched_at >= ?",
  args: [Math.floor(Date.now()/1000) - 86400],
  instruction: 'Was waren die Top-3 Diskussionspunkte der letzten 24 Stunden?',
  mode: 'haiku',
  max_tokens: 1000
})
// → summary auto-generated, no human-Claude needed
// Then cap_save_analysis to persist
```

Cost: ~500 input + ~300 output tokens at Haiku 4.5 pricing = fractions of a cent per call. Quality is sufficient for log-style summaries; for nuanced analysis use `mode: 'data'` with a conversation Claude instead.

## 7. Time-Windowed Retrieval

"What did I analyze last week?"

```js
await cap_search({
  cap: 'reddit_fetch',
  table: 'analyses',
  where: "created_at >= ? AND created_at < ?",
  args: ['2026-04-11', '2026-04-18']
})
```

`created_at` is auto-populated by `cap_store` as ISO 8601 UTC (DATETIME). Lexicographic string comparison works for date ranges.

## 8. Source-Filter Introspection ("which posts have I already analyzed?")

Avoid duplicate work. Before analyzing post X, check:

```js
const prior = await cap_search({
  cap: 'reddit_fetch',
  table: 'analyses',
  where: "filter_where LIKE '%post_permalink%' AND filter_args LIKE ?",
  args: ['%' + targetUrl + '%']
})
if (prior.count > 0) {
  // Already analyzed — surface the prior summary instead of re-running
}
```

Handy when conversation-Claude is deciding whether to fetch + summarize anew or reference a stored analysis.

## Common Pitfalls

### Forgetting to supersede when schema changes

When you update a capability's JSON schema (e.g. add a new param), save with `supersedes: <old_id>`. Older threads still see the old tool until they call `activate_capability` again. Without supersede, you'll have two active versions and unpredictable routing.

### Passing literal `'NULL'` strings vs SQL NULL

cap_store stores what you pass. If you want a real SQL NULL, omit the field in the upsert `data` object — don't pass `null` (JSON encoding) or `'NULL'` (string).

### Forgetting `all: true` on large caps

`cap_search` defaults to 100 rows. For full-table analysis use `all: true, max_rows: 5000` (or higher). Be aware that cap_collect defaults to 500 rows and truncates quietly (check `res.truncated`).

### WHERE sanitizer rejecting legitimate queries

If `cap_search` returns `Error: blocked SQL keyword`, check your WHERE clause for unquoted SQL keywords. Wrap in quotes: `body LIKE '%delete%'` works; `body LIKE %delete%` fails (no quotes).

### Model tag after whoami fix

Once session_model proxy-state is populated (post this commit), `mcp__yesmem__whoami` returns `model`. But until the MCP session_id-resolution bug is fixed separately, whoami still returns empty session_id — meaning the model field won't surface via MCP. Work around by passing `model` explicitly to `cap_save_analysis`.

## Schema Evolution

When you need a new field in `analyses` (say, `cost_usd`), two options:

1. **Add a column** — requires raw SQL `ALTER TABLE cap_<cap>__analyses ADD COLUMN cost_usd REAL`. There's no current cap for this; use `sqlite3` directly or add a daemon RPC.
2. **Overload `tags`** — store `cost_usd:0.004` as a pseudo-tag. Quick, no schema change, but inefficient for range queries.

Option 1 is the right long-term answer. Option 2 is a reasonable stopgap.

## Related

- `cap-store-analysis.md` — API reference + architecture
- `internal/storage/cap_store.go` — underlying storage
- Learning #53633 — the cap_save_analysis v2 schema fix (column collision)

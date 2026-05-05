# Documentation

Technical documentation for YesMem.

## Contents

- **[config-reference.md](config-reference.md)** — Complete reference for all `config.yaml` settings
- **[settings-json-reference.md](settings-json-reference.md)** — YesMem-relevant `settings.json` entries: MCP server, hooks, permissions
- **[CapFeatures.md](CapFeatures.md)** — Capability memory system: design, architecture, implementation status
- **[CAPS-md-spec.md](CAPS-md-spec.md)** — CAP.md file format specification (YAML frontmatter + Markdown)
- **[caps-vs-skills-rationale.md](caps-vs-skills-rationale.md)** — Rationale for caps vs. skills as a standalone format
- **[cap-store-analysis.md](cap-store-analysis.md)** — Queryable analysis layer on top of `cap_store` SQLite tables
- **[cap-store-analysis-examples.md](cap-store-analysis-examples.md)** — End-to-end walkthroughs for cap_store analysis
- **[JobsFeature.md](JobsFeature.md)** — Built-in job scheduler: recurring/one-shot tasks, three execution modes
- **[BENCHMARK.md](BENCHMARK.md)** — LoCoMo benchmark methodology, reproduction steps, and cost estimates
- **[sawtooth-cost-analysis.md](sawtooth-cost-analysis.md)** — Proxy cost analysis: sawtooth collapsing pattern, real 24h production data
- **[cache-keepalive-cost-analysis.md](cache-keepalive-cost-analysis.md)** — Prompt cache keepalive economics: TTL bridging, ping costs, break-even analysis

## See Also

- **[Features.md](../Features.md)** — Complete technical reference (50 MCP tools, ~130 RPCs, 53 CLI commands)
- **[README.md](../README.md)** — Project overview, install, architecture

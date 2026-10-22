# mnemonic at scale, as open source

Status: proposal, Oct 21 2026. Nothing here is approved or scheduled. It comes from a research swarm over Braintrust, moraine, the open-source LLM observability tools, trace storage engines, the trace standards and the coding-agent tools, read against the code at `29e6c37`.

## The recommendation in one paragraph

Keep what mnemonic already is: one Go binary, raw lines as the truth, everything else derived and rebuildable, recall served to agents over MCP. Add four things, in this order. First, a span tree derived from the events we already parse. Second, OTLP in and ATIF/OTLP out, so other tools can feed it and read from it. Third, a tenant above the author. Fourth, immutable segments on object storage behind the Store seam already planned. Do not add ClickHouse, Redis, a queue or a UI. The tools that did need 5 to 16 services to self-host, and one binary that runs on a laptop, a box or a bucket is the thing nobody else ships for agent traces.

## What the field does

| Project | Store | Ingest | Self-host footprint | License |
|---|---|---|---|---|
| Braintrust Brainstore | Rust binary; all data on object storage, WAL compacted into Tantivy in the background, Postgres metadata, Redis locks, partition per customer [1] | SDK, OTLP traces (10 MB cap, 413) [2] | gated, license key [3] | closed |
| Langfuse v3 | ClickHouse for traces, Postgres, S3, Redis [4] | SDK, OTLP HTTP (no gRPC) | 6 compose services [5] | MIT core, `ee/` for RBAC and SCIM [6] |
| Opik | ClickHouse, MySQL, Redis, MinIO, Zookeeper | SDK, OTel | 16 compose services [7] | Apache-2.0 |
| Laminar | ClickHouse, Postgres, Quickwit | SDK, OTel | 5 (lite) [8] | Apache-2.0, license key for signals |
| Arize Phoenix | SQLite default, Postgres optional [9] | OTLP; converts `gen_ai.*` to OpenInference at ingest [10]; imports ATIF [11] | one process | ELv2 [12] |
| Pydantic Logfire | parquet on object storage, DataFusion, Postgres metadata only [13] | OTel | n/a | SDK open |
| Grafana Tempo | parquet blocks on object storage, `meta.json` written last [14] | OTLP | microservices or single binary | AGPLv3 [15] |
| moraine (upstream) | ClickHouse | file tailing, 11 harnesses | ClickHouse server | Apache-2.0, 116 stars, BM25 only [16] |

Three designs carry the lessons that apply to us.

- **Brainstore.** "All data lives on object storage." Compute is stateless. Writes are "strongly consistent and visible immediately" because reads merge the WAL [1]. Their own benchmark: 3.9M documents of about 25 KB, full-text search 240 ms against 78,963 ms for an unnamed warehouse on the same c7gd.8xlarge. They attribute the gap to the warehouse skipping rows with bloom filters, which they call slow for targeted text search [1]. That is a vendor number, not ours.
- **Logfire.** For each parquet file, Logfire pulls the 128 largest JSON keys into typed columns at ingest and compaction. Types widen on conflict, `Int` to `Float` to `JSON`. 99% of their files have fewer than 128 top-level keys [13]. This is how to make `payload_json` analytics fast without a global schema.
- **Tempo.** Write data first and metadata last, so a reader never sees a half-written block [14].

Interchange is converging, though none of it is stable yet.

- **OTel GenAI.** Spans are `invoke_agent`, `create_agent`, `chat` and `execute_tool`, plus `gen_ai.conversation.id`. Message content goes in `gen_ai.input.messages` and `gen_ai.output.messages`, which are "Opt-In" and flagged PII. Every item is at "Development" status [17].
- **OTLP.** Version 1.11.1 is stable over HTTP/JSON and protobuf, at `/v1/traces` and `/v1/logs`, with partial success and a recommended 64 MiB request cap [18].
- **ATIF.** Harbor's trajectory format (RFC Active, April 2026) uses `steps[]` with `source`, `message`, `tool_calls` and `observation`. Phoenix imports it [11][19].
- **Agent Trace.** Cursor's Jan 2026 RFC attributes code ranges to conversations [20].

The coding harnesses export OTel themselves. Content is off by default:

- **Claude Code** emits `claude_code.user_prompt`, `tool_result`, `api_request` and cost/token metrics. Prompts are `<REDACTED>` unless `OTEL_LOG_USER_PROMPTS=1` [21].
- **Codex** has an `[otel]` exporter whose `log_user_prompt` defaults to false [22].
- **Gemini CLI** logs prompts by default [23].
- **Cursor and opencode** have no OTel; they have hooks or on-disk sessions [24].

## Where mnemonic stands

What is already right, and is the precedent everyone else converged on:

- **Raw is truth, the rest is derived.** Parquet and the cache rebuild from raw, and a parser change bumps that parser's `Version()` (`AGENTS.md`, `internal/normalize/normalize.go`).
- **Redaction on the client** (`internal/redact`).
- **The author comes from the token,** never the request (`internal/auth/auth.go`).
- **Lake partitioned by author, machine, harness** (`internal/lake/lake.go:521`).
- **The push protocol is already a generic append-only stream.** A source is `src_id` + `offset` + `head_hash`. An append must start at the server's size or get a 409. Offset 0 replaces the source (`internal/wire/wire.go:22-73`). Nothing in it needs the source to be a file on the client.
- **Parsers are a registry** over `io.Reader` with a resumable `State` (`internal/normalize/normalize.go:23-43`).

## Lies table

| Lie underneath | The one fix | What gets deleted |
|---|---|---|
| A trace is a flat list of events. `trace_id` is only filled for Claude Code and Cursor (`claudecode.go:204`, `cursor.go:126`). The tree lives implicitly in `parent_tool_call_id`, `agent_run_id`, `turn_index` (`internal/schema/events.go:33-48`). | A `spans` relation in the cache, derived from the columns each parser already writes: session root, turn, model call, tool call (request and response paired), subagent. OTel GenAI operation names as the span kinds. | Per-tool tree rebuilding in `open` and the prose formatters once they read `spans`. |
| Only files can feed it. | An OTLP/HTTP receiver that appends each request's spans and logs as JSON lines to a server-side source (`harness=otlp`, one source per tenant, service and day), parsed by one more registered parser. Same raw, lake and cache path. | Nothing. No second pipeline exists. |
| The author is the tenant. | Token maps to (tenant, author, scopes). The lake path gains `tenant=` above `author=`. Each session gets a visibility (private, team, unlisted link), like Amp's after it ended public threads [25]. | The implicit one-team-per-server assumption in config. |
| One DuckDB file is the store. Every tool takes `*sql.DB` (`internal/query/query.go:13`). One process, 1 GB memory limit (`cmd/mnemonic/server.go:49`). | The Store seam from the Oct 21 outline (S3 there), then immutable search segments and lake files on object storage. A manifest commits last (Tempo). Readers merge the uncompacted tail (Brainstore). DuckDB stays as the SQL engine over the lake, not as the store. | `cache/index.duckdb` as the search index, after a shadow period. |
| A trace is only read, never judged. | An append-only annotations stream (score, label, feedback, outcome such as tests passed or the commit made) keyed by session and span. Scores are records, never in-place overwrites; Braintrust writes each scorer run as its own span [26]. | Nothing. |

## Target design

### Data model

- **events** (unchanged contract). One row per parsed raw line, 70 columns.
- **spans** (new, derived). Fields: `tenant`, `session_id` (= `gen_ai.conversation.id` on export), `span_id`, `parent_span_id`, `kind`, `name`, `start`, `end`, `status`, `model`, token counts, `cost` when a source reports it, and the first and last `event_uid`.
  - `kind` is one of `invoke_agent`, `chat`, `execute_tool`, `turn`.
  - `span_id` is a hash of the source span key, stable across re-parses, so annotations survive a parser bump.
- **annotations** (new, raw stream). `target` (session or span), `name`, `value` (number, label or text), `author`, `source` (human, scorer, CI), `ts`. They are pushed through the same wire protocol as their own source.
- **links** (new, derived). Session to file (what `file_attention` already computes), and session to commit, from Agent Trace records [20] or a checkpoint trailer like Entire's [27].

### Capture

1. **File tailing** stays the primary path for coding harnesses. It is the only one that gets full content by default, works without harness support, and avoids the hook docs' warning that the transcript may lag [28].
2. **OTLP receiver** (`POST /v1/traces`, `/v1/logs`, JSON and protobuf, gzip, partial success). It covers SDK-built agents (OpenLLMetry, OpenInference, Claude Agent SDK, OpenAI Agents) and the harnesses' own OTel. Claude Code's events carry `message.uuid`, which the docs say refers to messages in the `~/.claude/projects/*/*.jsonl` transcripts, plus a `session.id` that the docs do not say equals the transcript's [21]. The join keys on `message.uuid`, so cost and `api_request` latency attach to existing sessions. Claude Code's span export is beta behind `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1` [21].
   - All `gen_ai.*` and `openinference.*` names live in one mapping file, because the conventions are still Development.
   - Content attributes are stored only when the tenant opts in, the same default the spec takes [17].
3. **Hooks** are not a capture path of their own. A hook's best use is to tell `push --watch` to flush now.

### Store

- **Raw.** Today's append-only files, later seekable-zstd frames. The Oct 21 outline measured about 4.4x on real traces with 64 KB frames at level 3. Raw goes on local disk or object storage behind one interface.
- **Lake.** Parquet per (tenant, author, machine, harness), base plus tail, as now. Add Logfire-style per-file shredding of `payload_json`'s largest keys, so analytics over tool inputs stop parsing JSON.
- **Search.** Pure-Go postings segments with block-max MAXSCORE (Oct 21 outline S2). These are immutable, one set per tenant, with a manifest written last. Brainstore's numbers are the argument for a real inverted index over bloom-filter skipping when the query is ranked full-text. Blooms (Tempo, VictoriaLogs) fit needle filters on logs, not BM25 recall.
- **Large payloads.** Tool outputs over a cap are stored once by content hash and referenced from the event. Search indexes their first N KB. Braintrust caps attachments at 20 MB and keeps JSON attachments out of search [29].
- **Deletion.** Two kinds:
  - a delete request is a record;
  - queries filter it at once, and compaction drops it from segments and lake files after a cancel window.

  Loki works this way, with a 24 h default window and per-tenant modes [30]. Raw deletion is the same rewrite of the source.
- **Redaction version.** Each source records the rule version its bytes were redacted under, so a rule change can find what to re-redact. Today a stored prefix is never re-redacted, an accepted risk recorded Oct 20.

### Query

- **The five MCP tools stay verbatim** (`internal/mcp/tools.json`).
- **New MCP tools:**
  - `get_trace`, a span tree with token and cost totals;
  - `annotate`, which writes an annotation;
  - `query`, read-only SQL over the lake and `spans`. It requires a time range and a row cap, and returns 429 with Retry-After when busy. Braintrust and Logfire both bound queries this way: a 30 s sync timeout and a required range filter for Braintrust [31], a 100 default and 10,000 max rows for Logfire [32].
- **HTTP export:**
  - ATIF per session, validated against Harbor's rules [19];
  - OTLP forwarding to any collector, so a team can also see sessions in Langfuse, Phoenix or Braintrust;
  - JSONL datasets filtered by annotation, with a per-task cap. SWE-smith keeps at most 3 trajectories per instance [33].

### Deployment modes, one binary, one format

1. **`mnemonic local`.** A laptop, one user, no server. It tails, indexes into a local data dir and serves MCP on localhost. This is the zero-config door. Phoenix's SQLite default is the precedent [9].
2. **`mnemonic serve`.** One box, the team server. This is today's mode.
3. **`mnemonic serve --store s3://...`.** Raw, lake and segments in a bucket. Writers append, compactors run in the background, readers are stateless and scale out. This is Brainstore's shape [1].

The data layout is the same in all three, so moving up a mode is a copy.

## Open source

- **License.** Stay Apache-2.0.
  - It is what we inherit from moraine (`NOTICE`), and CNCF sandbox projects must use it [34].
  - The alternatives come with a cost:
    - ELv2 (Phoenix) bars offering the software as a managed service [12];
    - AGPL (Grafana since 2021) was a business-strategy choice [15];
    - FSL turns into Apache-2.0 or MIT only after two years [35].
- **Contributions.** Use DCO, not a CLA. DCO keeps each contributor's copyright and lowers the barrier, as GitLab did in 2017 [36]. No `ee/` folder.
- **Publish the contracts as specs:**
  - the push protocol (`internal/wire`);
  - the events and spans columns;
  - the MCP tool surface;
  - the parser golden format.

  Others can then write a producer or a parser without reading Go.
- **Upstream.** Offer the parser and redaction fixes back to moraine. The goldens already come from it (`testdata/`).
- **Positioning (Inferred).** No project found does this combination: self-hosted, many harnesses, raw-transcript recall to agents over MCP. The neighbours each cover part of it:
  - claude-mem stores per-user observations in SQLite and Chroma, captured by hooks [37];
  - Entire stores checkpoints in git [27];
  - Langfuse and Braintrust watch LLM apps through SDKs;
  - moraine needs ClickHouse.

## Staged path

Each stage ends on a countable check against the real server, never a self-report.

| Stage | Done when |
|---|---|
| S0 replay harness (Oct 21 outline) | a recorded day of pushes and probes replays against a lab and prints p50, p95 and errors |
| S3 Store seam | every tool reads through the seam, with no `*sql.DB` in `internal/query`; the replay matches byte for byte |
| P1 spans + `get_trace` + ATIF export | 100 sampled sessions per harness validate as ATIF; one imports into Phoenix and renders |
| P2 OTLP receiver | Claude Code and Codex OTel exports land and join their transcript sessions (Claude Code by `message.uuid`); cost totals appear in `get_trace` |
| P3 annotations + `query` | an annotation written over MCP is readable after a parser-version sweep; `query` rejects a missing range |
| P4 tenant + visibility + delete requests | two tenants on one server cannot read each other (scripted probe); a delete request disappears from search at once and from disk after compaction |
| S2 segments in shadow, then on object storage | shadow search matches DuckDB on the replay probes; one reader on another box serves from the bucket |
| P5 `mnemonic local` + public specs + release | a fresh machine goes from download to a working MCP recall in one command |

## What this does not build

- No ClickHouse, Redis, queue or Kubernetes chart.
- No UI before the specs and exports exist. Phoenix and Langfuse can render our traces through ATIF and OTLP.
- No embeddings yet. BM25 is the moraine baseline. The memory-benchmark numbers we found are disputed between vendors (Zep against mem0 on LongMemEval). Embeddings wait for a measured recall gap on our own probes.
- No extracted-memory layer (mem0, Graphiti, claude-mem style). Raw recall is the product; extraction can be a client of `query`.

## Owner calls

1. **Scope.** Coding-agent recall first with OTLP as the door (recommended), or general LLM-app observability.
2. **License and public date.** Apache-2.0 plus DCO (recommended). Also when the repo goes public and under which org.
3. **Defaults for OTLP content capture.** Off unless the tenant opts in (recommended, matches the spec).
4. **Relationship with moraine upstream.** Contribute back, or announce as its Go successor.
5. **Object store provider** for the bucket mode.

## Unknown

Searched and not found, or found only in one secondary source:

- **Braintrust:** merge semantics (`_is_merge`, `_xact_id`), the Dec 2025 benchmark, and its SDK and autoevals licenses.
- **Langfuse:** its v4 large-payload store and its deletion behaviour.
- **Phoenix, Opik, Laminar:** their data models and query APIs.
- **OTel GenAI:** the exact `execute_tool` and `chat` span name formats, and the current stability opt-in value.
- **Native transcript schemas** for SWE-agent, OpenHands and Codex.
- **Trace volumes** published by any vendor, embedding prices, and S3 prices checked at the source.
- **Zep's product docs** and Letta's retrieval internals.
- **Whether the parsers' current columns fully determine a span tree** for every harness. P1 has to prove it per harness before it can claim it.

## Sources

Accessed Oct 21 2026.

1. https://www.braintrust.dev/blog/brainstore (Mar 3 2025)
2. https://www.braintrust.dev/docs/kb/otlp-traces-endpoint-payload-limits.md
3. https://www.braintrust.dev/docs/admin/self-hosting/deploy.md
4. https://langfuse.com/blog/joining-clickhouse
5. https://raw.githubusercontent.com/langfuse/langfuse/main/docker-compose.yml
6. https://langfuse.com/self-hosting/license-key
7. https://raw.githubusercontent.com/comet-ml/opik/main/deployment/docker-compose/docker-compose.yaml
8. https://raw.githubusercontent.com/lmnr-ai/lmnr/main/docker-compose.yml
9. https://arize.com/docs/phoenix/self-hosting/architecture
10. https://www.arize.com/docs/phoenix/release-notes/05-2026/05-15-2026-otel-semconv-conversion.md
11. https://arize.com/docs/phoenix/release-notes/04-2026/04-03-2026-atif-trajectory-upload.md
12. https://github.com/Arize-ai/phoenix/blob/main/LICENSE
13. https://pydantic.dev/articles/dynamic-shredding-2026-01-26 and https://pydantic.dev/docs/logfire/deploy/self-hosted-deployment/architecture/
14. https://grafana.com/docs/tempo/next/reference-tempo-architecture/block-format.md
15. https://grafana.com/blog/grafana-loki-tempo-relicensing-to-agplv3/
16. https://github.com/eric-tramel/moraine
17. https://raw.githubusercontent.com/open-telemetry/semantic-conventions-genai/main/docs/gen-ai/gen-ai-spans.md and gen-ai-agent-spans.md in the same repo
18. https://opentelemetry.io/docs/specs/otlp/
19. https://raw.githubusercontent.com/harbor-framework/harbor/main/rfcs/0001-trajectory-format.md
20. https://agent-trace.dev
21. https://code.claude.com/docs/en/monitoring-usage
22. https://learn.chatgpt.com/docs/config-file/config-advanced
23. https://geminicli.com/docs/cli/telemetry/
24. https://cursor.com/docs/agent/hooks and https://opencode.ai/docs/troubleshooting/
25. https://ampcode.com/news/end-of-public-threads
26. https://www.braintrust.dev/foundations/online-scoring
27. https://docs.entire.io/core-concepts
28. https://code.claude.com/docs/en/hooks
29. https://www.braintrust.dev/docs/instrument/attachments.md
30. https://grafana.com/docs/loki/latest/operations/storage/logs-deletion/
31. https://www.braintrust.dev/docs/reference/sql.md
32. https://pydantic.dev/docs/logfire/how-to-guides/query-api/
33. https://arxiv.org/html/2504.21798
34. https://github.com/cncf/sandbox/blob/main/README.md
35. https://fsl.software/
36. https://about.gitlab.com/blog/2017/11/01/gitlab-switches-to-dco-license
37. https://github.com/thedotmack/claude-mem

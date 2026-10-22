# Spans and annotations

An [event](events.md) list is flat; a trace is a tree. A span is one node of that tree: a session, a turn, a model request, a tool call, a subagent. The server derives spans from events in the index and never stores them in the lake, so a change to a derivation re-derives them without re-parsing anything.

An annotation is a judgment attached to a session or a span: a score, a label or a note, written by a person, a scorer or a CI job.

Related specs: [events](events.md), [wire](wire.md), [mcp](mcp.md), [otlp](otlp.md).

## The spans relation

`spans` is a table in `cache/index.duckdb` (`internal/schema/spans.go:52-70`). One row per span.

| Column | Type | Meaning |
|---|---|---|
| `tenant` | VARCHAR | the owner of the data; today the constant `default` (`internal/cache/index_spans.go:50`) |
| `session_id` | VARCHAR | the [event](events.md) `session_id` |
| `span_id` | VARCHAR | first 16 hex characters of `sha256` over the span's source key. It is the same after every re-parse. |
| `parent_span_id` | VARCHAR | the parent's `span_id`; `""` on the session root |
| `kind` | ENUM | `invoke_agent`, `chat`, `execute_tool` or `turn` (`internal/schema/spans.go:8-18`) |
| `name` | VARCHAR | a label for display |
| `start_time`, `end_time` | TIMESTAMP | the smallest and largest `event_time` of the span's events (`internal/cache/index_spans.go:31-34`) |
| `status` | ENUM | `ok` or `error` (`internal/schema/spans.go:20-27`) |
| `model` | VARCHAR | the model of a `chat` span, `""` otherwise |
| `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` | UBIGINT | token counts; see the claude-code rules below |
| `cost` | DOUBLE | null when the source reports none. The claude-code derivation always leaves it null. |
| `first_event_uid`, `last_event_uid` | VARCHAR | the `event_uid` of the span's first and last event in `event_order` |

The kinds follow the OpenTelemetry GenAI operation names: `invoke_agent` for an agent run, `chat` for a model request, `execute_tool` for a tool call, and `turn` for one user prompt and everything that follows it (`internal/schema/spans.go:8`).

### Span ids

A `span_id` is `substr(sha256(concat_ws(U+001F, parts...)), 1, 16)` in lowercase hex, where the parts are the span's source key (`internal/cache/index_spans.go:25-28`). For claude-code the key is a tag followed by ids from the events (`internal/cache/index_spans_claudecode.go:24-40`):

| Span | Key parts |
|---|---|
| session root | `session`, `session_id` |
| turn | `turn`, `session_id`, `turn_seq` |
| subagent | `agent`, `session_id`, the `agent_run_id` |
| chat | `chat`, `session_id`, the `request_id` (else `trace_id`) |
| tool call | `tool`, `session_id`, the `tool_call_id` |

### When spans are built

Spans for a session are deleted and rebuilt whenever the session's events change, in the same step that recomputes `event_order` and `turn_seq` (`internal/cache/index.go:703-748`). A derivation sees only the events of one harness, matched by the `harness=<h>` segment of the lake path (`internal/cache/index_spans.go:46-52`). A harness with no derivation has no spans; today only `claude-code` has one (`internal/cache/index_spans_claudecode.go:3`).

The index records a fingerprint of all derivations. When the set changes, opening the index re-derives every session's spans without a full rebuild (`internal/cache/index_spans.go:54-83`).

### Writing a derivation

A derivation is registered per harness name with `cache.RegisterSpans(harness, derive)` from an `init` (`internal/cache/index_spans.go:18-23`). `derive(where)` returns one `SELECT` that yields every column of the table except `tenant`, matched by name; `where` selects the events of the sessions being rebuilt, with `event_order` and `turn_seq` already current (`internal/cache/index_spans.go:12-17`). The `events` table in the index has no `payload_json`, so a derivation reads values from columns ([events](events.md)).

### claude-code tree

`internal/cache/index_spans_claudecode.go:5-13` states the rules and `TestClaudeCodeSpanTree` (`internal/cache/index_spans_test.go:11`) pins them.

- The session is the root `invoke_agent`, named `claude-code`, with no parent.
- Each `turn_seq` of the main thread (rows with `is_substream = 0`) is a `turn` span named `turn N`, under the root.
- Each `agent_run_id` seen on a substream row is an `invoke_agent` span named by `agent_label`, else by the id. It sits under the tool call that its rows name in `parent_tool_call_id`, else under the root.
- Each `request_id` is a `chat` span named by the model, else `chat`. Its parent is its subagent, else its turn, else the root. Every row of a request repeats the request's usage, so a chat's tokens are the maximum over its rows.
- Each `tool_call_id` is an `execute_tool` span named by `tool_name`, else `tool`. Its parent is the chat that requested it, else the tool call named by `parent_tool_call_id`, else its subagent, turn or root, in that order. Its `status` is `error` when any row has `tool_error` above 0, else `ok`; its tokens are 0.
- The root, each turn and each subagent carry the sum of the tokens of the chats beneath them. Their `status` is `ok`.

## Annotations

An annotation is one append-only line of JSON (`internal/schema/annotations.go:110-120`):

```json
{"id":"a1","target":{"session_id":"S","span_id":"optional"},"name":"helpfulness",
 "value":{"number":0.8},"source":"scorer","ts":"2026-10-21T12:00:00Z"}
```

| Field | Rule |
|---|---|
| `id` | required; unique per annotation |
| `target.session_id` | required |
| `target.span_id` | optional; when set, the annotation judges that span of the session |
| `name` | required; what is being judged |
| `value` | an object with exactly one key: `number` (a float), `label` (a string) or `text` (a string). Zero or two keys is rejected. |
| `source` | one of `human`, `scorer`, `ci` |
| `ts` | required RFC 3339 timestamp |

`ParseAnnotation` rejects a line that breaks any rule, and unknown keys inside `value` (`internal/schema/annotations.go:83-107`, `internal/schema/annotations.go:122-144`). The author is not in the line; it is the author of the source the line was appended to, so a caller can annotate only as itself (`internal/schema/annotations.go:110-111`).

### The annotations relation

The index holds a table `annotations`, rebuilt from the lake files of each changed annotation source (`internal/schema/annotations.go:146-159`, `internal/cache/index_annotations.go:28-49`).

| Column | Type | From |
|---|---|---|
| `annotation_id` | VARCHAR | `id` |
| `target_session_id` | VARCHAR | `target.session_id` |
| `target_span_id` | VARCHAR | `target.span_id`; null when absent |
| `name` | VARCHAR | `name` |
| `value_number`, `value_label`, `value_text` | DOUBLE, VARCHAR, VARCHAR | the one key of `value`; the other two are null |
| `source` | ENUM | `source` |
| `author` | VARCHAR | the source's author, from the lake path |
| `ts` | TIMESTAMP | `ts`, as UTC |
| `lake_file`, `lake_row` | VARCHAR, UBIGINT | where the row came from |

Annotations never enter `events`, the search index, `sessions` or `turns` (`internal/cache/index_annotations.go:28-30`).

### How an annotation reaches the server

Annotations are written to sources the server owns, not pushed by a client (see "Sources owned by the server" in [wire](wire.md)). The raw lines of such a source are annotation JSON, one per line, in a source whose harness is `annotation`. Its parser follows this row contract (`internal/schema/annotations.go:9-22`): one event per line, with `event_uid` the annotation `id`, `session_id` the target session and `payload_json` the annotation as JSON. The harness name `annotation` and the right to write it are reserved for server-owned sources.

The tool that writes annotations is defined by the change that adds it and, like every tool, appears in `tools/list` ([mcp](mcp.md)); this page defines only the data.

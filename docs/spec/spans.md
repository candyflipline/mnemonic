# Spans and annotations

An [event](events.md) list is flat; a trace is a tree. A span is one node of that tree: a session, a turn, a model request, a tool call, a subagent. The engine derives spans from events as a part section, built with the part and read at query time, so a change to a derivation re-derives them when the part rebuilds and nothing re-parses.

An annotation is a judgment attached to a session or a span: a score, a label or a note, written by a person, a scorer or a CI job.

Related specs: [events](events.md), [wire](wire.md), [mcp](mcp.md), [otlp](otlp.md).

## The spans section

`schema.Span` (`internal/schema/spans.go`) is one span. A part holds the spans of its sessions in a section written at build (`internal/index/spans.go`, `buildSpans`); `get_trace` and the exports read them through the DB (`internal/index/spans.go`, `sessionSpans`; `internal/index/export.go`, `ExportSpans`). A span has no tenant column: whose session a span belongs to comes from `session_authors`, which every read filters on (`internal/index/scope.go`, `sessionAuthors`).

| Field | Type | Meaning |
|---|---|---|
| `SessionID` | string | the [event](events.md) `session_id` |
| `SpanID` | string | first 16 hex characters of `sha256` over the span's source key. It is the same after every re-parse. |
| `ParentSpanID` | string | the parent's `span_id`; `""` on the session root |
| `Kind` | SpanKind | `invoke_agent`, `chat`, `execute_tool` or `turn` (`internal/schema/spans.go`) |
| `Name` | string | a label for display |
| `Start`, `End` | time.Time | the smallest and largest `event_time` of the span's events (`internal/index/spans.go`, `spanBounds`) |
| `Status` | SpanStatus | `ok` or `error` (`internal/schema/spans.go`) |
| `Model` | string | the model of a `chat` span, `""` otherwise |
| `InputTokens`, `OutputTokens`, `CacheReadTokens`, `CacheWriteTokens` | uint64 | token counts; see the claude-code rules below |
| `Cost` | *float64 | nil when the source reports none. The claude-code derivation always leaves it nil. |
| `FirstEventUID`, `LastEventUID` | string | the `event_uid` of the span's first and last event in `event_order` |

The kinds follow the OpenTelemetry GenAI operation names: `invoke_agent` for an agent run, `chat` for a model request, `execute_tool` for a tool call, and `turn` for one user prompt and everything that follows it (`internal/schema/spans.go`).

### Span ids

A `span_id` is the first 16 hex characters of `sha256` over U+001F-joined key parts (`internal/index/spans.go`, `spanIDHex`). For claude-code the key is a tag followed by ids from the events (`internal/index/spans_claudecode.go`, the key builders):

| Span | Key parts |
|---|---|
| session root | `session`, `session_id` |
| turn | `turn`, `session_id`, `turn_seq` |
| subagent | `agent`, `session_id`, the `agent_run_id` |
| chat | `chat`, `session_id`, the `request_id` (else `trace_id`) |
| tool call | `tool`, `session_id`, the `tool_call_id` |

### When spans are built

A part section is a function of the part's members: build derives each session's spans from that session's winner rows of one harness while the part is written, and query time only reads (`internal/index/build.go`, the section build; `internal/index/DESIGN.md`, decision 3). A part rebuilds when a member joins or leaves it, so spans track their events through the same rebuild the events get. A harness with no derivation has no spans; `claude-code`, `codex`, `cursor`, `pi-coding-agent`, `prime-agent` and `otlp` have one (`internal/index/spans.go`, `spanBuilders`, and `internal/index/spans_<harness>.go`).

### Writing a derivation

A derivation is `func(harness string, session []*Row) []schema.Span`, registered per harness name in `spanBuilders` from an `init` (`internal/index/spans.go:22`). It receives one session's winner rows in event order with `turn_seq` and `event_ordinal` current, the rows the part stores; the heavy columns are not among them, so a derivation reads values from columns ([events](events.md)).

### claude-code tree

`internal/index/spans_claudecode.go` states the rules and `TestClaudeCodeSpanTree` (`internal/index/sections_golden_test.go`) pins them against the 59796bc oracle's tree.

- The session is the root `invoke_agent`, named `claude-code`, with no parent.
- Each `turn_seq` of the main thread (rows with `is_substream = 0`) is a `turn` span named `turn N`, under the root.
- Each `agent_run_id` seen on a substream row is an `invoke_agent` span named by `agent_label`, else by the id. It sits under the tool call that its rows name in `parent_tool_call_id`, else under the root.
- Each `request_id` is a `chat` span named by the model, else `chat`. Its parent is its subagent, else its turn, else the root. Every row of a request repeats the request's usage, so a chat's tokens are the maximum over its rows.
- Each `tool_call_id` is an `execute_tool` span named by `tool_name`, else `tool`. Its parent is the chat that requested it, else the tool call named by `parent_tool_call_id`, else its subagent, turn or root, in that order. Its `status` is `error` when any row has `tool_error` above 0, else `ok`; its tokens are 0.
- The root, each turn and each subagent carry the sum of the tokens of the chats beneath them. Their `status` is `ok`.

## Annotations

An annotation is one append-only line of JSON (`internal/schema/annotations.go`):

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

`ParseAnnotation` rejects a line that breaks any rule, and unknown keys inside `value` (`internal/schema/annotations.go`). The author is not in the line; it is the author of the source the line was appended to, so a caller can annotate only as itself.

### The annotations section

The engine holds annotations as a part section, built with the part (`internal/index/annotations.go`, `buildAnnotations`). An annotation source joins no other source's component (`internal/index/ingest.go`, `componentsOf`): it is a component of its own, so annotating a session builds only the annotation source's own small part and rewrites no part that holds the session's events. A session's annotations may therefore sit in the sections of several parts; a read merges them (`sessionAnnotations`). Within a part the section has one row per `annotation_id`, from the first row the build saw, the way the 59796bc cache qualified `row_number` over filename and file row number; across parts, the same `annotation_id` (the same annotation appended on two days, which the store's one-day look-back does not merge) is kept once, with the earliest `ts`. The row is the `schema.Annotation`: `id`, target, `name`, the one key of `value`, `source`, `ts` as UTC, and the source's `author` from its path.

Annotations never enter `events`, the search index, `sessions`, `turns` or a session's authors: the annotation rows feed only the section (`internal/index/build.go`, `buildIndex`). An annotator is therefore never a session author, so annotating changes neither who may see a session nor whether a `link` session has exactly one author ([mcp](mcp.md)).

### How an annotation reaches the server

Annotations are written to sources the server owns, not pushed by a client (see "Sources owned by the server" in [wire](wire.md)). The raw lines of such a source are annotation JSON, one per line, in a source whose harness is `annotation`. Its parser follows this row contract (`internal/schema/annotations.go`, the `AnnotationHarness` comment): one event per line, with `event_uid` the annotation `id`, `session_id` the target session and `payload_json` the annotation as JSON. The harness name `annotation` and the right to write it are reserved for server-owned sources.

The `annotate` tool writes them (`internal/index/q_annotate.go`); like every tool it appears in `tools/list` ([mcp](mcp.md)).

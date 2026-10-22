# Events

An event is one row of one thing an agent did or said: a message, a tool call, a tool result, a piece of reasoning, a system notice. A parser turns each raw line of a [pushed source](wire.md) into zero or more events. This page lists every column, its type and what fills it, and the golden format that pins the parsers. A stranger can write a parser, or read the parquet files, from it alone.

The column list is generated from `docs/clickhouse/events.tsv` into `internal/schema/events.go` and is a contract: it changes only with the owner (`AGENTS.md`, Contracts). Types below are the ClickHouse type in the TSV and the DuckDB type in `internal/schema/events.go:12-70`.

Related specs: [wire](wire.md), [spans](spans.md) (a tree derived from these columns), [mcp](mcp.md), [otlp](otlp.md).

## Where events live

| Place | What | Source |
|---|---|---|
| lake | one parquet per source, `events/author=A/machine=M/harness=H/<src_id>.parquet`, plus `<src_id>.tail.parquet` for rows appended since | `internal/layout/layout.go:1-9` |
| index | the `events` table of `cache/index.duckdb`, which is what MCP reads | `internal/cache/sql.go:80-98` |

The lake stamps three columns onto every parsed row, so the parquet has the 57 parser columns below plus these (`internal/lake/lake.go:621-628`):

| Column | Type | Meaning |
|---|---|---|
| `author` | String | the token's author, taken from the source's path and never from a payload |
| `machine_id` | String | the `machine` segment of the source's path |
| `ingested_at` | DateTime64(3) | when the lake wrote the row; rows imported from the retired ClickHouse store carry `1970-01-01 00:00:00.000` (`internal/lake/lake.go:31`) |

When two rows share an `event_uid` the one with the highest `event_version` wins (`internal/lake/lake.go:321-322`, `internal/cache/index.go:217`).

The index adds columns of its own: `doc`, `lake_file`, `lake_row`, `event_time`, `event_order`, `turn_seq`, `event_ordinal` and a few derived ones (`internal/cache/sql.go:80-98`). The index does not keep `payload_json` or `token_usage_json`; read those from the parquet (`internal/cache/sql.go:78`). The ordering columns:

| Column | Meaning |
|---|---|
| `event_time` | `record_ts` parsed as a timestamp, else `ingested_at` (`internal/cache/index.go:213`) |
| `event_order` | 1-based position in the session, ordered by `event_time`, `source_file`, `source_generation`, `source_offset`, `source_line_no`, `emission_index`, then `event_uid` (`internal/cache/index.go:692-704`) |
| `turn_seq` | `turn_index` when it is above 0; otherwise 1 plus the number of user messages seen so far, at least 1 (`internal/cache/index.go:699-701`) |
| `event_ordinal` | 1-based position within the turn |

## Columns

Dates and times are text in the parser output: a DateTime64(3) is `YYYY-MM-DD HH:MM:SS.mmm` in UTC and a Date is `YYYY-MM-DD` (`internal/schema/events.go:9-10`, `internal/normalize/shared/time.go:76-78`). A column a parser has nothing for is `""`, `0`, `[]` or a zero map; no column is null.

### Identity and time

| Column | Type | Meaning |
|---|---|---|
| `event_uid` | String | lowercase hex `sha256` over the event's meaning: the columns in `eventIdentityFields`, the token maps, the payload with volatile fields removed, and a 1-based occurrence number when two events of one line would otherwise match. It does not include the file, line or offset, so a parser fix that changes none of those fields keeps the uid (`internal/normalize/shared/hash.go:43-96`, `internal/normalize/shared/hash.go:129-146`). |
| `session_id` | String | the harness's own session id. claude-code reads `sessionId` from the record, else the file name when it is a UUID, else the id carried over from the previous line (`internal/normalize/claudecode/claudecode.go:46-62`). |
| `session_date` | Date | the `YYYY/MM/DD` in the source path when it has one, else the UTC date of `record_ts`, else `1970-01-01` (`internal/normalize/shared/time.go:38-46`) |
| `source_name` | LowCardinality(String) | the producer's label for the origin, the `source_name` of the push (`internal/normalize/shared/row.go:127`) |
| `harness` | LowCardinality(String) | the parser's name, such as `claude-code` |
| `inference_provider` | LowCardinality(String) | who served the model; a per-harness default, `anthropic` for claude-code (`internal/normalize/claudecode/claudecode.go:27`) |
| `record_ts` | String | the line's own timestamp, trimmed, as written; empty when the line has none, except that codex falls back to the timestamp in a `rollout-YYYY-MM-DDTHH-MM-SS-` file name (`internal/normalize/shared/pipeline.go:112-125`) |
| `event_ts` | DateTime64(3) | `record_ts` as UTC; `1970-01-01 00:00:00.000` when it does not parse. RFC 3339 and `YYYY-MM-DDTHH:MM:SS[.frac]` (taken as UTC) parse (`internal/normalize/shared/time.go:10`, `internal/normalize/shared/time.go:60-74`). |
| `event_version` | UInt64 | version of this parse of the row; the highest wins when a uid repeats (`internal/normalize/shared/pipeline.go:306`, `internal/lake/lake.go:321-322`) |

### Where the row came from

| Column | Type | Meaning |
|---|---|---|
| `source_file` | String | the producer's absolute path, the `path` of the push |
| `source_inode` | UInt64 | always 1 |
| `source_generation` | UInt32 | always 1 |
| `source_line_no` | UInt64 | 1-based line number, counting blank and unparseable lines |
| `source_offset` | UInt64 | byte offset of the start of the line in the stored (redacted) source |
| `source_ref` | String | `<source_file>:1:<source_line_no>` |

`source_inode` and `source_generation` are constants because a source is always the whole file (`internal/normalize/shared/row.go:11`, `internal/normalize/shared/row.go:80-82`). The line number and offset are those of `internal/normalize/shared/jsonl.go:13-20`. Lines that are blank or not JSON produce no event.

### What kind of event

`event_kind` and `payload_type` take values from fixed sets; anything else is stored as `unknown` (`internal/normalize/shared/row.go:84-117`).

| Column | Type | Meaning |
|---|---|---|
| `event_kind` | LowCardinality(String) | one of `session_meta`, `turn_context`, `message`, `tool_call`, `tool_result`, `reasoning`, `event_msg`, `compacted_raw`, `progress`, `system`, `summary`, `queue_operation`, `file_history_snapshot`, `unknown` (`internal/normalize/shared/row.go:98-103`) |
| `actor_kind` | LowCardinality(String) | who produced it: `user`, `assistant`, `tool` or `system` in every golden (`testdata/goldens/*.json`) |
| `payload_type` | LowCardinality(String) | the record's own type, one of the 30 values in `internal/normalize/shared/row.go:105-117`, such as `text`, `tool_use`, `tool_result`, `thinking`, `function_call`, `user_message` |
| `op_kind` | LowCardinality(String) | for operational records, the subtype (a claude-code `system` record's `subtype`, a `progress` record's `data.type`); `""` for ordinary messages (`internal/normalize/claudecode/claudecode.go:339-342`, `internal/normalize/claudecode/claudecode.go:363-371`) |
| `op_status` | LowCardinality(String) | the record's own `status` for operational records |
| `endpoint_kind` | LowCardinality(String) | `generation` for every row of every golden (`internal/normalize/shared/row.go:141`) |
| `content_types` | Array(String) | the block types in the content, such as `text`, `tool_use`, `tool_result`, `reasoning`, `image` |
| `has_reasoning` | UInt8 | 1 on a reasoning event |

### Correlation: turns, requests, tools, agents

| Column | Type | Meaning |
|---|---|---|
| `turn_index` | UInt32 | the harness's own turn number when it gives one (codex `turn_id`); 0 otherwise. The index derives `turn_seq` when it is 0 (`internal/normalize/codex/codex.go:119-121`). |
| `request_id` | String | the model request the event belongs to: claude-code `requestId`, codex `turn_id` |
| `trace_id` | String | claude-code `requestId` again |
| `item_id` | String | the harness's id for this record (claude-code `uuid`) |
| `tool_call_id` | String | links a tool call to its result; same value on both events |
| `parent_tool_call_id` | String | the tool call a nested record runs under (claude-code `parentToolUseID`); a subagent's rows name the call that started it |
| `tool_name` | LowCardinality(String) | name of the tool, set on the call event |
| `tool_phase` | LowCardinality(String) | `request` on a call and `response` on a result for tool events; for claude-code content blocks it carries the message's `stop_reason` (`internal/normalize/shared/pipeline.go:182`) |
| `tool_error` | UInt8 | 1 when the tool reported an error |
| `origin_event_id` | String | the `event_uid` of the event this one derives from (claude-code `sourceToolAssistantUUID`, rewritten to a uid) |
| `origin_tool_call_id` | String | the tool call this record answers (claude-code `sourceToolUseID`) |
| `agent_run_id` | String | the subagent run (claude-code `agentId`) |
| `agent_label` | String | the subagent's name (claude-code `agentName`) |
| `coord_group_id` | String | reserved; no parser sets it |
| `coord_group_label` | String | the team name (claude-code `teamName`) |
| `is_substream` | UInt8 | 1 when the row belongs to a subagent's own stream (claude-code `isSidechain`) |

### Model and tokens

| Column | Type | Meaning |
|---|---|---|
| `model` | LowCardinality(String) | the model name, canonicalized per harness (claude-code reads `message.model`, `internal/normalize/claudecode/claudecode.go:211`) |
| `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens` | UInt32 | the usage the line reports, saturating at 2^32-1 (`internal/normalize/shared/tokens.go:107-113`, `internal/normalize/shared/tokens.go:152-154`, `internal/normalize/claudecode/claudecode.go:212`). Every content block of one request repeats the request's usage, so sum per request, not per row. |
| `latency_ms` | UInt32 | claude-code `durationMs` on operational records |
| `retry_count` | UInt16 | claude-code `retryAttempt` on operational records |
| `service_tier` | LowCardinality(String) | the usage block's `service_tier` |
| `token_usage_json` | String | the harness's usage object, compacted |
| `token_usage_buckets` | Map(String, UInt64) | every key always present, zero when unused: `embedding_input_image`, `embedding_input_text`, `input_audio`, `input_cache_read`, `input_cache_write`, `input_image`, `input_text`, `other`, `output_audio`, `output_image`, `output_text`, `reasoning`, `server_tool_use` (`internal/normalize/shared/tokens.go:3-17`) |
| `token_usage_native_units` | Map(String, Float64) | every key always present: `input_audio_seconds`, `input_image_pixels`, `input_images`, `output_audio_seconds`, `output_image_pixels`, `output_images` (`internal/normalize/shared/tokens.go:19-26`) |

### Content

| Column | Type | Meaning |
|---|---|---|
| `text_content` | String | the text of the event, cut to 200,000 characters |
| `text_preview` | String | the first 320 characters of `text_content` (`internal/normalize/shared/convert.go:109`, `internal/normalize/shared/row.go:144`) |
| `payload_json` | String | the record or content block as compact JSON. On a tool event it also carries the tool's input and output under `moraine_tool_io` (`internal/normalize/shared/pipeline.go:193`). |

### Project

| Column | Type | Meaning |
|---|---|---|
| `cwd` | String | the working directory: the record's own, else the last one seen in the source |
| `project_id` | LowCardinality(String) | `git:` or `dir:` plus a sha256 of the repository's common git dir or of the directory, for the worktree that contains `cwd` or the tool's file path; `""` when none exists on the machine running the parser (`internal/normalize/shared/attention.go:207-219`) |
| `worktree_root` | String | the root that `project_id` was computed for |
| `repo_rel_path` | String | the file a tool call touched, relative to `worktree_root`; set only on tool requests whose input names exactly one path (`internal/normalize/shared/attention.go:44-60`) |

`project_id`, `worktree_root` and `repo_rel_path` are read from the parser's own file system. They are empty for a project whose directory the server cannot see, so they are never a join key across machines.

## Writing a parser

A parser is `Parse(r io.Reader, src SourceMeta, from State, emit func(*Event) error) (State, error)` (`internal/normalize/normalize.go:39-42`). It registers under its harness name and the lake calls it with the raw source (`internal/normalize/normalize.go:46-56`).

- `State` is where a parse stopped: next `LineNo`, byte `Offset`, and the session, model and cwd carried to the next line. Parsing the tail of a file from the `State` its head ended in must give the rows a whole-file parse gives those lines (`internal/normalize/normalize.go:19-32`).
- The reference helper `shared.ParseJSONL` does the line walking; a new harness supplies the per-record mapping (`internal/normalize/shared/jsonl.go:13-21`, `internal/normalize/shared/pipeline.go:8-18`).
- A parser change that changes any output must bump `normalize.Version`, and `TestVersionNamesOutput` fails until it does (`internal/normalize/normalize.go:11`, `internal/normalize/all/golden_test.go:83-100`). The server then re-parses every source parsed under another version.

Registered harnesses: `claude-code`, `codex`, `cursor`, `pi-coding-agent`, `prime-agent` (`internal/normalize/*/*.go`, each file's `Harness()`). A source whose harness has no parser is stored and counted `raw_only`, not parsed (`internal/lake/lake.go:130-133`).

## Golden format

`testdata/goldens/<name>.json` pins a parser against a fixture in `testdata/fixtures/`.

```json
{
  "harness": "codex",
  "name": "codex",
  "source_file": "/fixtures/codex/session.jsonl",
  "records": [
    {
      "event_rows": [ { "event_uid": "...", "session_id": "...", "...": "..." } ],
      "error_rows": [], "link_rows": [], "raw_rows": [], "tool_rows": [],
      "session_hint": "...", "model_hint": "...", "cwd_hint": "..."
    }
  ]
}
```

- `records` has one entry per input line that produced anything. `event_rows` is the list of event objects that line should yield, each an object with the column names above as keys (`testdata/goldens/codex.json`).
- The test parses the fixture, flattens every `records[].event_rows` in order, and requires the same count. For each golden row it then requires every key in the golden to equal the parser's value (`internal/normalize/all/golden_test.go:35-76`). Keys the golden omits are not compared.
- `event_version` is replaced by the literal `"<event_version>"` on both sides before comparing, because it changes on every parse (`internal/normalize/all/golden_test.go:66`).
- The other keys (`error_rows`, `link_rows`, `raw_rows`, `tool_rows` and the three hints) are kept from the upstream project's fixtures and are not checked by this repo's test.
- The parser is given the case's `sourceFile`, not the fixture's path on disk, so `source_file`, `source_ref` and the uids in a golden are stable across machines. The golden's own `source_file` key records the same string (`internal/normalize/all/golden_test.go:18-33`).
- The test has 10 cases. A case whose harness has no registered parser is skipped, and the log line reports how many ran (`internal/normalize/all/golden_test.go:36-41`, `internal/normalize/all/golden_test.go:75`). Four goldens in the directory (`cursor_sqlite`, `hermes_session_json`, `kimi_cli_subagent`, `nac`) have no case and are not run.

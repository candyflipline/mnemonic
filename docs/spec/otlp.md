# OTLP receiver

`mnemonic serve` accepts OpenTelemetry traces and logs over OTLP/HTTP, so an agent built on an SDK (OpenLLMetry, OpenInference, the OpenAI Agents or Claude Agent SDKs) or a harness with its own telemetry (Claude Code, Codex) can report to the server without a file on disk. Each span or log record becomes one [event](events.md), and the spans that carry a GenAI or OpenInference kind become rows of the [spans](spans.md) relation.

Related specs: [wire](wire.md), [events](events.md), [spans](spans.md), [mcp](mcp.md).

## Endpoints

| Request | Path | Body |
|---|---|---|
| traces | `POST /v1/traces` | `ExportTraceServiceRequest` |
| logs | `POST /v1/logs` | `ExportLogsServiceRequest` |

The paths are the OTLP/HTTP defaults; the logs path is `/v1/logs`, and `/v1development/` is the path of the profiles signal only (OTLP spec, "OTLP/HTTP Request"). Metrics are not accepted. Both routes sit behind the same bearer-token middleware as the push store (`internal/server/server.go:45-46`); an exporter sets `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer <token>`. The author a request writes as is the token's author (`internal/otlp/receiver.go:102`).

The handler (`internal/otlp/receiver.go:81-126`):

- Content types are `application/json` and `application/x-protobuf`; anything else is 415. The response uses the request's content type.
- `Content-Encoding: gzip` is accepted; any other encoding is 415 and a body that is not gzip is 400.
- The body, compressed or decompressed, is capped at 64 MiB (`MaxBody`, `internal/otlp/receiver.go:37`); above that the answer is 413 (`readBody`, `internal/otlp/receiver.go:129-152`).
- An undecodable body is 400. Every 4xx and 5xx body is a `google.rpc.Status` in the request's content type.
- A failed store write is 503, which OTLP exporters retry; the spec retries only 429, 502, 503 and 504.
- A request with no spans or records is 200 with an empty response.

### JSON encoding

OTLP/JSON writes `traceId`, `spanId` and `parentSpanId` as hex, and `protojson` reads `bytes` as base64. `idsToBase64` rewrites those three fields (camelCase or snake_case) to base64 before decoding (`internal/otlp/receiver.go:186-222`); a non-hex id is a 400. 64-bit integers may be decimal strings or numbers, and enums integers or names, both through `protojson`. Unknown fields are ignored.

### Partial success

A span is rejected when its trace id is not 16 non-zero bytes or its span id is not 8 non-zero bytes (`internal/otlp/convert.go:55-56`). A log record is rejected when it has no session (see below) and no trace id (`internal/otlp/convert.go:112-116`). The rest of the request is stored and the response is 200 with `partialSuccess.rejectedSpans` or `rejectedLogRecords` and an `errorMessage`. A request with nothing rejected leaves `partialSuccess` unset, as the spec requires.

## What is stored

The receiver appends the accepted records as JSON lines to a server-owned source through `store.Append` (`internal/store/append.go`). The source is per author, per `service.name` and per UTC day, named `otlp/<service>/<yyyy-mm-dd>` (`store.Today`, `internal/otlp/receiver.go:112`); `service.name` is lowercased and any character outside `a-z 0-9 . - _` becomes `_`, with `unknown_service` when it is missing (`internal/otlp/convert.go:161-179`). A client PUT naming harness `otlp` is refused with 403 (`internal/store/store.go:150`).

The request key is the sha256 of the decompressed body (`internal/otlp/receiver.go:100`), so an exporter's retry of the same body appends nothing, also across midnight (`store.Append` checks the previous day).

One line per record (`Record`, `internal/otlp/record.go:17`):

| Field | Meaning |
|---|---|
| `signal` | `span` or `log` |
| `session_id` | the session the record belongs to (below) |
| `trace_id`, `span_id`, `parent_span_id` | lowercase hex; a log carries the trace context it was emitted under |
| `name` | the span name, or a log's event name (`LogRecord.event_name`, else the `event.name` attribute) |
| `span_kind`, `status_code`, `status_message` | the OTLP enums as integers |
| `start_unix_nano`, `end_unix_nano` | decimal strings (spans) |
| `time_unix_nano`, `observed_unix_nano`, `severity_number`, `severity_text` | logs |
| `body` | a log's body, present only with `content_stored` |
| `events` | a span's events, each `name`, `time_unix_nano` and gated `attributes` |
| `attributes`, `resource`, `scope` | the record's attributes, its resource's attributes and its instrumentation scope; values are JSON, integers keep every digit |
| `content_stored` | the server's `otlp.store_content` when the line was written |

No receive time is stored, so the same record exported twice is the same line.

### Session

The session of a record is the first of `gen_ai.conversation.id`, `session.id`, `conversation.id` it carries, record attributes before resource attributes (`SessionKeys`, `internal/otlp/mapping.go`). A record without one takes the session of another record of its trace in the same request, else the trace id (`stampSessions`, `internal/otlp/convert.go:129`). The spans of one trace that arrive in different requests, only some of them naming a session, land in different sessions.

## The content gate

Message content is stored only when the server config has

```toml
[otlp]
store_content = false
```

set to `true` (`internal/config/config.go:60-65`); the default is false, which matches the OTel GenAI convention that message content SHOULD NOT be captured by default. With it false, the receiver removes the attributes in `Gates` (`internal/otlp/mapping.go:76`) from each span, span event and log record, and from resource attributes, before the first byte is written, so raw, parquet and the index never hold them. A log record's `body` is dropped too (`internal/otlp/convert.go:100-104`). A gate with `In` applies only to records named so, after removing a `claude_code.` or `codex.` prefix.

| Attributes | Why |
|---|---|
| `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions`, `gen_ai.tool.call.arguments`, `gen_ai.tool.call.result`, `gen_ai.tool.definitions` | OTel GenAI opt-in content |
| `gen_ai.prompt.*`, `gen_ai.completion.*` | OpenLLMetry's flattened prompts and completions |
| `input.value`, `output.value`, `llm.input_messages.*`, `llm.output_messages.*`, `llm.prompts.*`, `llm.prompt_template.*`, `tool.parameters`, `retrieval.documents.*`, `embedding.embeddings.*`, `reranker.input_documents.*`, `reranker.output_documents.*` | OpenInference content |
| `process.command_line`, `process.command_args` | a command line can carry a prompt |
| `prompt`, `prompt_text` on `user_prompt` | Claude Code and Codex prompt text |
| `user_prompt`, `new_context`, `system_reminders`, `system_prompt_preview`, `user_system_prompt`, `response.model_output` on `interaction` and `llm_request` spans; `response` on `assistant_response`; `body` on `api_request_body` and `api_response_body` | Claude Code prompts, model text and raw API JSON |
| every attribute but `event.name` on `system_prompt` events and `tool.output` span events | Claude Code system prompt and tool output |
| `tool_input`, `tool_parameters`, `full_command`, `file_path`, `hook_definitions`, and `error` on `tool_result` and `tool.execution` | Claude Code tool arguments and the full error text its `OTEL_LOG_TOOL_DETAILS` gate covers |
| `arguments`, `output` on `tool_result` | Codex tool arguments and output snippet |

The Claude Code and Codex names come from the Claude Code monitoring documentation (events and the beta trace spans) and `codex-rs/otel` (`user_prompt`, `tool_result`); the sources are in the `Source` column of `Gates`. Counts, sizes, durations, ids, names and model and token attributes are kept.

## Events

The parser `otlp` (`internal/normalize/otlp/otlp.go`) turns each line into one event; a line that is not JSON is skipped. `normalize.Version` is unchanged because no golden case covers the harness.

| Column | Value |
|---|---|
| `event_uid` | span: sha256 of `otlp\|span\|<trace id>\|<span id>`, the same for every export of the span; log: sha256 of `otlp\|log\|` and the stored line |
| `session_id` | `session_id` of the line |
| `harness`, `source_name`, `source_file` | `otlp`; the source's name and path, `otlp/<service>/<day>` |
| `inference_provider` | first of `gen_ai.provider.name`, `gen_ai.system`, `llm.provider`, `llm.system` |
| `record_ts`, `event_ts`, `session_date` | the start time (a log's time, else observed time), `record_ts` as RFC 3339 with microseconds |
| `event_kind` | `event_msg` for every record |
| `payload_type` | `span` or `log` |
| `op_kind` | `chat`, `execute_tool` or `invoke_agent` for a span of that kind, `span` for any other span, `log` for a log record |
| `op_status` | `ok` for status code 1, `error` for 2, `""` unset |
| `actor_kind` | `assistant` for `chat` and `invoke_agent`, `tool` for `execute_tool`, `""` otherwise |
| `trace_id` | the trace id |
| `item_id` | the span id (spans) |
| `origin_event_id` | the `event_uid` of the parent span; `""` at a root |
| `request_id` | first of `gen_ai.response.id`, `request_id` |
| `tool_name`, `tool_call_id` | first of `gen_ai.tool.name`, `tool.name`, `tool_name`; first of `gen_ai.tool.call.id`, `tool.id`, `tool_use_id`, `call_id` |
| `tool_error` | 1 on an `execute_tool` span with status code 2 |
| `agent_run_id`, `agent_label` | first of `gen_ai.agent.id`, `agent_id`; first of `gen_ai.agent.name`, `agent.name` |
| `model` | lowercased first of `gen_ai.response.model`, `gen_ai.request.model`, `llm.model_name`, `model` |
| `input_tokens`, `output_tokens` | first of `gen_ai.usage.input_tokens`, `llm.token_count.prompt`, `input_tokens`, `input_token_count`; first of `gen_ai.usage.output_tokens`, `llm.token_count.completion`, `output_tokens`, `output_token_count` |
| `cache_read_tokens`, `cache_write_tokens` | first of `gen_ai.usage.cache_read.input_tokens`, `llm.token_count.prompt_details.cache_read`, `cache_read_tokens`, `cached_token_count`; first of `gen_ai.usage.cache_write.input_tokens`, `gen_ai.usage.cache_creation.input_tokens`, `llm.token_count.prompt_details.cache_write`, `cache_creation_tokens`, `cache_write_token_count` |
| `token_usage_buckets` | `input_text`, `output_text`, `input_cache_read`, `input_cache_write` set from the same four numbers, the rest zero |
| `token_usage_native_units` | the zero keys of every parser, plus `cost_usd` when the record has one of `gen_ai.usage.cost`, `cost_usd` |
| `latency_ms` | a span's end minus start in whole milliseconds; a log's `duration_ms` |
| `text_content`, `text_preview` | the span name or event name; a stored log body when it is a string |
| `payload_json` | the stored line |

Tokens are stored as reported: `gen_ai.usage.input_tokens` includes cached tokens by the convention, while Claude Code's `input_tokens` excludes them. `cost_usd` rides in `token_usage_native_units` because the index keeps no other column that holds a float and the span derivation reads only index columns.

All mappings of attribute names live in `internal/otlp/mapping.go`; the conventions are in Development and a rename changes that file only.

## Spans

`cache.RegisterSpans("otlp", ...)` (`internal/cache/index_spans_otlp.go:3`) derives spans from the events of sessions whose lake file is under `harness=otlp`.

- A span row is an event with `payload_type = 'span'` and an `op_kind` of `chat`, `execute_tool` or `invoke_agent`. The kind is `gen_ai.operation.name` (`chat`, `text_completion`, `generate_content` to `chat`; `execute_tool`; `invoke_agent`, `create_agent` to `invoke_agent`), else `openinference.span.kind` (`LLM`, `TOOL`, `AGENT`), else the span name `claude_code.llm_request`, `claude_code.tool`, `claude_code.interaction` (`KindOf`, `internal/otlp/mapping.go:54`). Other spans are not emitted.
- `span_id` is the first 16 hex characters of sha256 over `otlp`, the trace id and the OTLP span id, joined by U+001F (the rule of [spans](spans.md)).
- `parent_span_id` is the nearest emitted ancestor along `origin_event_id`; a span whose ancestors are all other spans, or absent from the session, is a root with `""`.
- `name` is the span name, `start_time` the event time, `end_time` the start plus `latency_ms`, `status` `error` when `op_status` is `error`.
- A `chat` and an `execute_tool` carry their own tokens, model and cost. An `invoke_agent` carries the sum over the `chat` spans beneath it, at any depth, and their cost when any has one; with no chat beneath it, its own figures. `cost` is null when no record reports one.
- `first_event_uid` and `last_event_uid` are the span's own event.

Log records are events only. A Claude Code `api_request` log carries cost and tokens but names no span, so its cost does not reach `spans`.

## Not done

- Joining Claude Code's `message.uuid` to transcript sessions, so cost and `api_request` latency attach to files already pushed.
- Metrics, and OTLP/gRPC.
- A per-tenant `store_content`; the setting is server-wide.

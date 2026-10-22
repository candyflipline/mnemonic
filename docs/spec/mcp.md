# MCP interface

How to talk to a mnemonic server from an MCP client, or to write a client, without reading Go. The server speaks MCP over plain HTTP: one endpoint, JSON-RPC 2.0, no streaming and no sessions. The tool surface is the one the upstream project's MCP 0.7.3 `tools/list` returns (`AGENTS.md`, Contracts), served verbatim, plus any tools listed after it.

Related specs: [wire](wire.md) (how data gets in), [events](events.md) (what the tools read), [spans](spans.md), [otlp](otlp.md).

## Endpoint and authentication

`POST /mcp` on the server's address (`internal/server/server.go:42`). `mnemonic serve` listens on the address in its config; `mnemonic local` listens on `127.0.0.1:7781` unless told otherwise, refuses any address that is not loopback, and prints the URL at start (`cmd/mnemonic/local.go:50`, `cmd/mnemonic/local.go:121`).

Every request carries `Authorization: Bearer <token>`. A missing or unknown token is `401 unauthorized` (`internal/auth/auth.go:24-33`). The caller's author comes from the token and is what a tool sees as the caller (`internal/auth/auth.go:39-42`).

## Request rules

The server answers these before it looks at the JSON-RPC body, in this order (`internal/mcp/mcp.go:119-172`). Each failure is an HTTP status with the body `{"error":"<message>"}` (`internal/mcp/mcp.go:395-399`).

| Check | Failure |
|---|---|
| method is `POST` | `405` with `Allow: POST` |
| `Content-Type` is `application/json` (parameters such as charset allowed) | `415` |
| `Accept` names `application/json` with a q-value other than 0; `*/*` does not count | `406` |
| `Origin`, when present, is an http or https URL whose host is a loopback IP address | `403`. A DNS name is rejected, so a web page cannot reach a local server. |
| body is at most 32 MiB | `413` |
| body is one JSON value | `400` |
| `jsonrpc` is `"2.0"` | `400` |
| `id` is absent, null, a string or a number; `params` is absent, null, an object or an array | `400 invalid JSON-RPC request shape` |
| `method` is a non-empty string | `400` |
| `Mcp-Protocol-Version` header names a supported version; required on every method except `initialize` | `400 missing or unsupported MCP-Protocol-Version` |

Supported protocol versions: `2024-11-05`, `2025-03-26`, `2025-06-18`, `2025-11-25`, `2026-07-28` (`internal/mcp/mcp.go:53-59`).

## Methods

A request without an `id` is a notification: the answer is `202` with an empty body, whatever the method. A request with an `id` is answered `200` with `{"jsonrpc":"2.0","id":<same id, same number formatting>,"result":...}` or `"error":{"code":N,"message":"..."}` (`internal/mcp/mcp.go:106-117`, `internal/mcp/mcp.go:175-222`).

| Method | Result |
|---|---|
| `initialize` | `{"protocolVersion":V,"capabilities":{"tools":{"listChanged":false}},"serverInfo":{"name":"mnemonic","version":"..."}}`. V is the requested `params.protocolVersion` if supported, else `2024-11-05` (`internal/mcp/mcp.go:295-311`). |
| `notifications/initialized`, `initialized` | `202` |
| `ping` | `{}` |
| `tools/list` | `{"tools":[...]}`: the entries of `internal/mcp/tools.json`, then those of `internal/mcp/tools_ext.json`, each entry's bytes unchanged (`internal/mcp/mcp.go:21-45`, `internal/mcp/mcp.go:291-293`). Each entry is `{name, description, inputSchema, annotations}`; `inputSchema` is JSON Schema and is the authority on arguments. |
| `tools/call` | a tool result, below |
| anything else | error `-32601` `method not found: <method>` |

A `tools/call` whose `params` do not decode as `{"name": string, "arguments": object}` is error `-32602` `invalid params: ...` (`internal/mcp/mcp.go:231-238`). Missing `arguments` is treated as null.

## Tool results

A call that reaches a tool is a JSON-RPC success whose `result` is (`internal/mcp/mcp.go:100-105`, `internal/mcp/mcp.go:231-282`):

```json
{"content":[{"type":"text","text":"<prose for a model>"}],
 "structuredContent":{ ...envelope... },
 "isError":false}
```

Success envelope (`internal/mcp/mcp.go:73-80`):

| Field | Value |
|---|---|
| `schema_version` | `mnemonic.mcp.<tool>.v1` |
| `tool` | the tool name |
| `request` | the `arguments` as sent |
| `data` | the tool's structured answer; its shape is per tool |
| `warnings` | array of strings, possibly empty |
| `performance` | `{"elapsed_ms": N}` |

Tool failures are still `200` with `"isError":true`. The prose says `<tool> error (<code>): <message>` and `structuredContent` is an error envelope with `schema_version` `mnemonic.mcp.error.v1`, the same `tool`, `request`, `warnings` and `performance`, and `error: {"code","message"}` in place of `data`. Codes (`internal/mcp/mcp.go:257-263`):

| Code | Meaning |
|---|---|
| `invalid_request` | the caller's arguments were wrong; the message says how |
| `internal_error` | the server failed |

An unknown tool name is `isError:true` with only the prose `unknown tool: <name>` and no `structuredContent` (`internal/mcp/mcp.go:243-246`, `internal/mcp/mcp.go:284-289`).

## Ids

`open`, `search_sessions` and `list_sessions` hand back typed ids and accept them (`internal/query/common.go:36-110`):

| Form | Meaning |
|---|---|
| `session:<b64>` | `<b64>` is the unpadded URL-safe base64 of the event `session_id` |
| `turn:<b64>:<n>` | the session, and the turn number `n` (the index's `turn_seq`) |
| `event:<b64>` | the base64 of the `event_uid` |

An id without one of those prefixes, with bad base64, or with non-UTF-8 bytes inside, is an `invalid_request`. Cursors are opaque: pass back the `next_cursor` you were given.

## The tools

All five are read-only (`"readOnlyHint": true`). Arguments are in each tool's `inputSchema`; this is what each is for.

| Tool | Required | Use |
|---|---|---|
| `search_sessions` | `query` | keyword (BM25, bag of words) search over events. By default only `user_input` and `assistant_response` events match; `event_types` widens that. Filters: `harness`, `source`, `within_id` (a session or turn id), `n_hits`. Returns event ids to `open`. |
| `open` | none (`id` or `cursor`) | summary of a session, turn or event by id. With `limit` it expands a page of turns or events and returns `next_cursor`. A `cursor` is passed alone. |
| `list_sessions` | `start_datetime`, `end_datetime` | sessions whose activity overlaps the range (RFC 3339 with explicit zone, end exclusive). Filters: `harness`, `source`, `mode`; `sort`, `limit`, `cursor`. |
| `file_attention` | `path` | every session that touched a file, across worktrees, matched by repository-relative path tail. Filters: `tool`, `mutations_only`, `scope`, `harness`, `source`, time bounds; `granularity` `sessions` or `events`. |
| `get_ingest_status` | none | ingestion health, coverage and freshness; use it before concluding something was never recorded. |

`harness` values are the parser names in [events](events.md), and `source` values are the push `source_name` labels. The lists the schemas print are fixed text and are not a promise about what a given server holds.

Tools added after these appear in `tools/list` after `get_ingest_status`, from `tools_ext.json`. This page does not describe them; their `inputSchema` and `description` do. The text part of a result is prose written for a model, its wording is not a stable interface, and `structuredContent.data` is.

## One call, start to finish

```
POST /mcp
Authorization: Bearer <token>
Content-Type: application/json
Accept: application/json
Mcp-Protocol-Version: 2025-06-18

{"jsonrpc":"2.0","id":1,"method":"tools/call",
 "params":{"name":"search_sessions","arguments":{"query":"inspect"}}}
```

The answer is `200` with `result.structuredContent.data.results[]`, each hit carrying an `event` object whose `id` is `event:...`, to pass to `open`.

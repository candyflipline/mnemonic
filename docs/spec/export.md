# Export over HTTP

`mnemonic export` is a client of a running `mnemonic serve` or `mnemonic local`. The server answers from the index snapshot it serves, pinned for the whole response (`internal/export/http.go`, `Handler`), and writes the body a step, span or dataset row at a time: an ATIF trajectory is validated step by step before its first byte, then built again and written. The CLI streams the body to `--out` or stdout and never opens an index.

## Route

`GET /v1/export/{kind}`, behind a key with the `read` scope.

| kind | params | body |
|---|---|---|
| `atif` | `session` (raw id or `session:<base64>`) | one ATIF trajectory, indented JSON |
| `otlp` | `session` | one `ExportTraceServiceRequest`, indented JSON |
| `dataset` | `annotation` (`name` or `name=value`), `per_task` (default 3) | JSONL, one row per session, newest first |

Every kind takes `include_private=true`.

| status | when |
|---|---|
| 200 | the body follows; a dataset session that failed ATIF validation is named in the `Mnemonic-Skipped` trailer as `<session id>: <error>` |
| 400 | unknown kind, missing or malformed params |
| 401 | no Identity on the connection |
| 403 | the key lacks `read`, or `include_private` without an admin key over every author |
| 404 | the session is not one the key may open, is deleted, or is not in the index |
| 422 | ATIF validation failed (atif), or every matching dataset session failed |
| 500 | anything else, before the first body byte; an error after it aborts the response, so the client sees a broken body and never a short 200 |

## Who sees what

The key's identity decides, as for the MCP read tools (`internal/index/scope.go`, `Sessions`): `atif` and `otlp` export a session the key can open with `open_session`, and `dataset` the sessions it can list. A deleted session is never exported. No read tool shows another author's private session, so `include_private` takes a key with the `admin` scope whose tenant is every author (a server with one tenant); it then exports every session that is not deleted, private ones included.

## CLI

```
mnemonic export atif|otlp --session S [--out F]
mnemonic export dataset --annotation N[=V] [--per-task K] [--include-private] [--out F]
```

The server, `server_key` and `key_file` come from the client config (`--config`, default `~/.mnemonic/config.toml`), as push takes them, and the request goes over the same QUIC connection `mnemonic mcp` uses (`cmd/mnemonic/export.go`, `cmd/mnemonic/quic.go`). `--local` asks a `mnemonic local` over plain HTTP on `127.0.0.1:7781`, or on `--listen`; local mode identifies every connection itself, so the request carries no credentials.

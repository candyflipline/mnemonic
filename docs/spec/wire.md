# Push protocol

How a producer sends raw transcript lines to a mnemonic server, so that a stranger can write one without reading Go. Everything here is what the code does today; each fact names the line that does it. A server is `mnemonic serve` or `mnemonic local`; they speak the same protocol.

A producer needs three things: a bearer token, a stable name for each file it sends, and the discipline to send whole lines. The server keeps one raw file per source and one manifest entry per source. The manifest is the only sync state: the server tells the producer where each source stands and the producer sends only what is missing.

Producers push over one gRPC stream ([The push stream](#the-push-stream)). The HTTP `GET` and `PUT` below are an adapter that stays only until 2026-12-01 ([The HTTP adapter](#the-http-adapter-expires-2026-12-01)).

Related specs: [events](events.md) (what the server makes of the lines), [spans](spans.md), [mcp](mcp.md), [otlp](otlp.md) (a second way in, for OpenTelemetry exporters).

## Authentication

Every `/v1/*` request, and the push stream, carries a bearer token. HTTP sends it as `Authorization: Bearer <token>`. gRPC sends the same value as `authorization` request metadata; both reach `tokens.Require(auth.Push)`, so there is one auth path. The server stores only `sha256(token)` as lowercase hex (`internal/auth/auth.go:120-123`) and maps it to an author. A missing or unknown token is `401 unauthorized` (`internal/auth/auth.go:143`), and a known token without the scope the route needs is `403 token lacks scope <scope>` (`internal/auth/auth.go:149`). On the stream they reach the client as gRPC `Unauthenticated` and `PermissionDenied`. The author a request writes as is the token's author, never a field of the request (`internal/auth/auth.go:1-3`, `internal/store/store.go:104`).

## Identifiers

| Name | Rule | Source |
|---|---|---|
| segment | `[a-z0-9._-]+`, and not `.` or `..`. Used for machine, harness and src_id. | `internal/wire/wire.go:15-19` |
| machine | a segment naming the sending host; the producer chooses it. A machine or harness reserved for server-owned sources is refused with 403 (see below). | `internal/store/store.go:150-153` |
| harness | a segment naming the parser that reads the source, such as `claude-code` or `codex`. A harness with no registered parser is stored but not parsed. `annotation`, `otlp` and `control` are reserved for server-owned sources and refused with 403 (see below). | `internal/index/ingest.go`, the parser lookup |
| src_id | first 16 hex characters of `sha256(path)`, where `path` is the absolute path of the file on the producer. The same path always yields the same src_id. | `internal/wire/wire.go:21-26` |
| head_hash | lowercase hex `sha256` of the source's first line, newline included, cut to the first 65536 bytes. It identifies the content: a file rewritten from the top changes it. | `internal/wire/wire.go:13`, `internal/wire/wire.go:28-36` |

The head hash is taken over the redacted first line, because redacted bytes are what the server holds (`internal/push/scan.go:43`).

## Lines

A source is a sequence of lines, each ending in `\n`. The producer sends whole lines only and redacts secrets before sending; the server does not redact. The reference client reads the file, cuts it at each `\n`, runs the redactor over each line, and drops a trailing partial line until its newline arrives (`internal/push/scan.go:19-32`). A redaction that changes no secret returns the line byte for byte (`internal/redact/redact.go:1-8`).

The offsets in this protocol count redacted bytes (`internal/wire/wire.go:45`), except `raw_end`, which counts the producer's own unredacted bytes.

## The push stream

The push protocol is one bidirectional gRPC call, `mnemonic.push.v1.Push/Sync`. [`internal/wire/pushpb/push.proto`](../../internal/wire/pushpb/push.proto) is the source of truth for every message and field; read it for the field lists. This section says what the messages mean and in what order they flow. The design and its reasons are in [docs/design/uploader.md](../design/uploader.md).

### Connect

The client dials the server's one listener over HTTP/2 and sends `authorization: Bearer <token>` as metadata. Its first message is `Hello`, carrying its machine name. The author is the token's, never a field.

### Welcome and the window

The server answers with `Welcome` pages, 4096 sources each. A `SourceState` holds what the skip rules read: `src_id`, `harness`, `size`, `head_hash`, `raw_end` and `updated_ms`. `more` is true on every page but the last. The last page carries the `Window`: how many appends (64) and how many bytes (96 MiB) the client may have sent and not yet seen acked. Both numbers are fixed constants, not configuration. The client sends no `Append` before the last page has arrived.

### Append

An `Append` is one HTTP `PUT` as the sections below define it, with the same meaning for `offset`, `head_hash`, `raw_end` and the body: offset 0 with a body replaces the source, offset above 0 with a body extends it, and offset above 0 with no body records `raw_end`. `seq` starts at 1 and rises by 1. The body is whole redacted lines, at most 32 MiB; the server accepts messages up to 33 MiB. The client may send a file's chunks back to back without waiting, because each chunk's offset is the previous chunk's end.

### Ack

The server sends exactly one `Ack` per `Append`, with the same `seq`, in send order.

- An ack means the append's bytes and manifest are durable on the server's disk. The server never acks earlier. Appends that arrive together share one group commit and are acked together after it.
- `APPLIED`: the append was applied, and `state` is the source after it.
- `CONFLICT`: nothing was written. `state` is the source as stored, with size 0 and no head hash when there is none. Every later in-flight append of that source conflicts too, because its offset no longer matches. The client re-decides that source from `state`.

A request the protocol forbids has no per-append error code. A bad segment, a reserved machine or harness, or a malformed body ends the stream with `InvalidArgument` or `PermissionDenied`. A correct client never sends one.

### Flow control

The client stops sending while 64 appends or 96 MiB are unacked. The server holds a bounded number of requests per stream and a global byte budget for frames waiting to commit. When the disk is slow the budget fills, the server stops reading, and HTTP/2 flow control stops the client's writes. Nothing is dropped; the next Welcome says what landed.

### Keepalive

The client pings about every 30 s so a proxy or NAT idle timeout never closes a stream that lives for hours.

### Resume after a disconnect

The client keeps nothing on disk. When a stream ends for any reason it backs off (5 s up to 1 min), redials, reads `Welcome`, and runs one full pass over its files with the skip rules in [How the reference client uses it](#how-the-reference-client-uses-it). Appends that were sent and never acked are forgotten; `Welcome` shows what landed. Sending the same bytes again converges on the same store.

### One stream per machine

A second stream for the same author and machine replaces the first: the newest stream wins, and the older one ends with gRPC `Aborted`.

## The HTTP adapter (expires 2026-12-01)

`GET /v1/manifest` and `PUT /v1/raw/...` below remain for clients that have not moved to the stream. They share the server's checks and write path with the stream, so they behave as described. The adapter and its handlers are deleted on 2026-12-01.

## GET /v1/manifest?machine=M

Returns what the server holds for the caller's author and machine `M`.

```
200 {"author":"A","machine":"M","sources":[Source, ...]}
```

`sources` is sorted by `src_id` and is `[]` when there are none (`internal/store/store.go:69-98`). A machine that is not a valid segment is `400 bad machine` (`internal/store/store.go:76-79`).

`Source` (`internal/wire/wire.go:40-49`):

| Field | Meaning |
|---|---|
| `src_id` | as above |
| `harness` | as sent in the path |
| `source_name` | the producer's label for the origin, such as `claude` or `codex` (the `source_name` query parameter) |
| `path` | the producer's absolute path (the `path` query parameter) |
| `size` | redacted bytes received. It is the `offset` the next append must carry. |
| `raw_end` | bytes of the producer's raw file consumed into `size`; `0` means unknown |
| `head_hash` | head hash of the stored content |
| `updated_ms` | server clock, Unix milliseconds, of the last change |

## PUT /v1/raw/{machine}/{harness}/{src_id}

Query parameters (`internal/wire/wire.go:60-66`, `internal/store/store.go:112-132`):

| Parameter | Required | Meaning |
|---|---|---|
| `offset` | yes, integer >= 0 | where in the stored source this body starts |
| `head_hash` | yes | head hash of the content the body belongs to |
| `source_name` | yes | label stored in the manifest |
| `path` | yes | producer path stored in the manifest |
| `raw_end` | no | offset in the producer's raw file where the body ends. A value that does not parse counts as `0`; a negative one is `400 bad raw_end`. |

The body is `application/octet-stream`: whole lines, each ending in `\n`. The reference client keeps one body under 32 MiB by cutting it at a line boundary (`internal/push/push.go:217-229`); the server sets no limit of its own (`internal/store/store.go:133`).

### Rules

The server checks in this order (`internal/store/store.go:100-163`):

1. Bad token: `401`.
2. A path segment that is not a valid segment: `400 bad path segment`.
3. `offset` missing, not an integer, or negative: `400 bad offset`.
4. `head_hash`, `source_name` or `path` missing: `400 missing query parameter`.
5. Empty body at `offset` 0, or a non-empty body that does not end in `\n`: `400 body must be non-empty whole lines`.
6. The reserved machine, or a reserved harness (`annotation`, `otlp`, `control`) on any machine: `403`.
7. `offset` is 0: replace (below).
8. `offset` is above 0: append (below).

Replace (`internal/store/store.go:181-210`). The body becomes the whole source. The previous manifest is deleted first and the raw file swapped second, so a crash between them leaves no manifest that disagrees with the raw file (`internal/store/store.go:165-179`). The answer is `200` with the new `Source`. Sending the same replace again leaves the same content and size.

Append (`internal/store/store.go:212-251`):

- No manifest for the source: `409` with `{"size":0,"head_hash":"","raw_end":0}`. The producer then sends a replace from offset 0.
- `offset` differs from the server's `size`, or `head_hash` differs from the stored one: `409` with the server's current state, `{"size":N,"head_hash":"H","raw_end":R}` (`internal/wire/wire.go:69-74`). The producer re-reads `size` and `head_hash` from the body and retries.
- Both match and the body is non-empty: the body is appended, `size` grows by its length, `raw_end` becomes the request's, and the answer is `200` with the updated `Source`.
- Both match and the body is empty: nothing is stored. If `raw_end` is above 0 it is recorded, and `size` and `updated_ms` stay the same. The answer is `200` with the `Source`. This is how a producer records where in its raw file the stored bytes end, so the next pass can skip unchanged files.

### How the reference client uses it

On the stream, `Welcome` supplies the server's state and the client sends each decision as an `Append`. The decision is the same for each local file (`internal/push/push.go:231-245`):

- If the server has no entry, its `head_hash` differs from the file's, or the file's redacted length is below the server's `size`, send everything at `offset=0`.
- If the redacted length equals `size`, there is nothing to send.
- Otherwise find the line boundary at byte `size` in the redacted file and send the rest at `offset=size`. When `size` falls inside a line, which happens when redaction re-encoded it, there is no safe seam and the client replaces.
- Over HTTP, after a `409`, take the answer's `size` and `head_hash` and try once more; a second `409` is an error (`internal/push/push.go:166-172`). On the stream a `CONFLICT` ack carries that state and the client re-decides at its next flush.

A producer that appends only, never rewrites, and computes its own head hash can ignore all of this except: read the server's state (`Welcome`, or the manifest over HTTP), send `offset=size` with the same `head_hash`, and on `409` start over at `offset=0`.

## Sources owned by the server

Some sources are written by the server itself, such as those fed by [OTLP](otlp.md) or by annotations. They live under a machine name reserved for server-owned sources. A client PUT or stream `Append` that names that machine, or one of the harnesses `annotation`, `otlp` and `control` on any machine, is refused with `403 machine or harness is reserved for server-owned sources` (`internal/store/store.go:150-153`, `internal/store/append.go:24`), so no producer can write there. The reserved names are fixed by the server and may grow; treat any machine or harness your server documents as server-owned as unavailable to you.

## What the server does next

Each committed change marks its source dirty and wakes the ingest loop, which reads only the changed sources' raw files (a full listing happens once, at boot), runs the harness's parser and ingests the [events](events.md) into the engine's store MCP reads (`internal/index/ingest.go`, `internal/index/handle.go`). A producer does not take part in that. A parser fix re-parses that harness's sources from raw, and no other's, so the raw lines are the durable thing: send them as the harness wrote them.

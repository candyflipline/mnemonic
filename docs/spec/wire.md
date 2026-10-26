# Push protocol

How a producer sends raw transcript lines to a mnemonic server, so that a stranger can write one without reading Go. Everything here is what the code does today; each fact names the line that does it. A server is `mnemonic serve` or `mnemonic local`; they speak the same protocol.

A producer needs three things: a bearer token, a stable name for each file it sends, and the discipline to send whole lines. The server keeps one raw file per source and one manifest entry per source. The manifest is the only sync state: the server tells the producer where each source stands and the producer sends only what is missing.

Producers push over one gRPC stream ([The push stream](#the-push-stream)); it is the only client path in. The HTTP `PUT` and manifest `GET` that used to sit beside it were deleted in this deploy, after the PUT census found a single client, which moved to the stream.

Related specs: [events](events.md) (what the server makes of the lines), [spans](spans.md), [mcp](mcp.md), [otlp](otlp.md) (a second way in, for OpenTelemetry exporters).

## Authentication

The push stream carries a bearer token as `authorization` request metadata, and it reaches `tokens.Require(auth.Push)`. The server stores only `sha256(token)` as lowercase hex (`internal/auth/auth.go:120-123`) and maps it to an author. A missing or unknown token is gRPC `Unauthenticated` (`internal/auth/auth.go:143`), and a known token without the push scope is gRPC `PermissionDenied` (`internal/auth/auth.go:149`). The author a stream writes as is the token's author, never a field of the request (`internal/auth/auth.go:1-3`, `internal/store/store.go:104`).

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

An `Append` carries one chunk of one source: `offset` is where the body starts in the stored source, `head_hash` the head hash of the content it belongs to, `source_name` the producer's label for the origin, `path` the producer's absolute path, and `raw_end` the offset in the producer's raw file the body ends at (`0` = unknown). Offset 0 with a body replaces the source; offset above 0 extends it and must equal the source's stored `size` with a matching `head_hash`, else the server answers `CONFLICT` with the stored state; offset above 0 with no body records `raw_end` only. `seq` starts at 1 and rises by 1. The body is whole redacted lines, at most 32 MiB; the server accepts messages up to 33 MiB. The client may send a file's chunks back to back without waiting, because each chunk's offset is the previous chunk's end.

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

The client keeps nothing on disk. When a stream ends for any reason it backs off (5 s up to 1 min), redials, reads `Welcome`, and runs one full pass over its files with the skip rules in [How the reference client decides](#how-the-reference-client-decides). Appends that were sent and never acked are forgotten; `Welcome` shows what landed. Sending the same bytes again converges on the same store.

### One stream per machine

A second stream for the same author and machine replaces the first: the newest stream wins, and the older one ends with gRPC `Aborted`.

## How the reference client decides

`Welcome` supplies the server's state and the client sends each decision as an `Append`. The decision is the same for each local file (`internal/push/push.go:113-126`):

- If the server has no entry, its `head_hash` differs from the file's, or the file's redacted length is below the server's `size`, send everything at `offset=0`.
- If the redacted length equals `size`, there is nothing to send.
- Otherwise find the line boundary at byte `size` in the redacted file and send the rest at `offset=size`. When `size` falls inside a line, which happens when redaction re-encoded it, there is no safe seam and the client replaces.
- A `CONFLICT` ack carries the stored state, and the client re-decides that source at its next flush.

A producer that appends only, never rewrites, and computes its own head hash can ignore all of this except: read the server's state from `Welcome`, send `offset=size` with the same `head_hash`, and on `CONFLICT` start over at `offset=0`.

## Sources owned by the server

Some sources are written by the server itself, such as those fed by [OTLP](otlp.md) or by annotations. They live under a machine name reserved for server-owned sources. A client stream `Append` that names that machine, or one of the harnesses `annotation`, `otlp` and `control` on any machine, is refused with `403 machine or harness is reserved for server-owned sources` (`internal/store/store.go:150-153`, `internal/store/append.go:24`), so no producer can write there. The reserved names are fixed by the server and may grow; treat any machine or harness your server documents as server-owned as unavailable to you.

## What the server does next

Each committed change marks its source dirty and wakes the ingest loop, which reads only the changed sources' raw files (a full listing happens once, at boot), runs the harness's parser and ingests the [events](events.md) into the engine's store MCP reads (`internal/index/ingest.go`, `internal/index/handle.go`). A producer does not take part in that. A parser fix re-parses that harness's sources from raw, and no other's, so the raw lines are the durable thing: send them as the harness wrote them.

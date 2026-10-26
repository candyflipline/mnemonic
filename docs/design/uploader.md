# Uploader: push stream, group commit, cycle dirty set

Candidate A of the `/architect` exploration, sketched on `lane/upl-arch-a` off main 83b0042. Sketch files (bodies panic, tricky logic as `// TODO` pseudocode):

| Piece | File |
|---|---|
| Proto and generated code | `internal/wire/pushpb/push.proto`, `push.pb.go`, `push_grpc.pb.go`, `gen.go` |
| Committer, batch state machine, dirty set | `internal/store/commit.go`, `barrier_linux.go`, `barrier_other.go` |
| Server transport (stream) | `internal/intake/intake.go` |

(The sketch's HTTP `PUT`/manifest `GET` adapter is not in the build: the PUT census found a single client ever (`users-mac-mini`), it moved to the stream, and the adapter was deleted in this deploy — the rollback pair is the 83b0042 server binary and that client's old one.)
| Client stream | `internal/push/stream.go` |
| Index cycle over a dirty set | `internal/index/lap.go` |

## Problem

Three facts make one durable append expensive and one cycle slow. The client learns the server's state by pulling the whole manifest, about 8.9k entries for the owner's Mac (`internal/push/push.go:67-91`), and again every 6 h (`internal/push/watch.go:26`); each file is then one PUT round trip with a 409 re-decide (`push.go:166-172`). The server makes each PUT its own transaction with three fsyncs: the frame (`internal/store/store.go:319`), the manifest temp file (`store.go:361`) and the directory (`store.go:371`). A replace costs five (`store.go:177, 288, 319, 361, 371`). And each index cycle lists and reads every manifest (`internal/index/ingest.go:109-126`), reads every checkpoint (`ingest.go:167-178`), and opens every raw file to hash a 64 KB window before deciding it is unchanged (`ingest.go:276-297, 330-333`). Prod runs put cycles of 7 to 8.4 s back to back with 8995 of 9001 sources unchanged (journal, Oct 26 02:20Z, from the packet).

Constraints the shape honours:

- C1: store files must stay byte-identical to the HTTP path's. Frame bytes depend only on the body and the zstd encoder defaults (`store.go:304-327`), and file bytes depend on how the client chunks. So the client keeps today's chunk cutter (`decide`, `firstChunk`, `rawEndOf`, `push.go:217-250`), and the server writes one frame per append as today.
- The author comes from the token, never the request (`internal/auth/auth.go:126-133`, `store.go:104`).
- Local mode has no transport and stays as it is. It still uses the store for annotations and controls (`cmd/mnemonic/local.go:133`), so it gets the new committer and keeps its behaviour.
- The client is pure Go, `CGO_ENABLED=0`. grpc is pure Go.

## Usage (caller's view)

The client, inside the watcher loop that exists today (`internal/push/watch.go:101`):

```go
u, err := dial(ctx, cfg)                  // Hello{machine}; Welcome pages -> u.sources
for _, f := range scan(cfg.Sources) {     // the pass a (re)connect starts with
    streamSource(ctx, red, u, f, &st)     // today's skip + cut; sends without waiting
}
for {
    select {
    case a := <-u.acks():                 // in send order, each after its fsync
        if u.applyAck(a) { dirty[a.path] = true } // conflict: re-decide next flush
    case err := <-u.ended():              // backoff 5 s .. 1 min, dial, full pass
    case p := <-n.changed:                // file events, 1 s quiet / 5 s max, as today
    }
}
```

The server, in `server.New`, one line replacing the `/v1/` mount (`internal/server/server.go:53`):

```go
st, err := store.Open(dataDir)            // loads every manifest once, starts the committer
push, drain := intake.Mount(st, tokens) // native Push server; shared push-scope check
// Serve push on its own listener; stop accepting, drain, then close st.
```

The server-owned writers do not change (`internal/otlp/receiver.go:132`, `internal/index/controls.go:175`, `internal/index/q_annotate.go:202`):

```go
src, dup, err := st.Append(store.Today(author, harness, stream), requestKey, lines)
```

The cycle loop (`server.go:92-119`):

```go
lap, stats, err := s.ix.Boot(dataDir, opts)        // the only full listing
for {
    select {
    case <-st.Changed(): settle(ctx, st.Changed()); reason = "put"
    case <-time.After(due): reason = "settle"       // hot -> cold is time-driven
    }
    stats, err = lap.Next(st.TakeChanged(), opts)
}
```

Inside the stream handler, the only path to an ack:

```go
t, err := st.Submit(ctx, src, op)   // parse at the edge first: NewClientSource, ParseOp
a, err := t.Wait(ctx)               // returns after the batch's last barrier
msg, err := ackOf(seq, a)           // takes a store.Ack; nothing else makes one
```

## Shape

### Data first

- `store.Op` is a sum type: `Replace`, `Extend`, `Backfill`, plus the unexported `serverAppend` (`internal/store/commit.go`). It replaces the offset/body flag combinations `servePut` branches on (`store.go:138-162`). Only `ParseOp` builds a client op and only `ParseLines` builds `Lines`, so "non-empty whole lines" is checked once at the edge (per boundary-discipline).
- `store.ClientSource` wraps a `layout.Key` whose segments are valid and not server-owned. `NewClientSource(author, ...)` is its only constructor. The 403 rule (`store.go:150-153`) lives there once for both transports.
- `store.Ack` has unexported fields, and the zero value is invalid. Outside `store`, an Ack exists only as `Ticket.Wait` returns it, and `Wait` returns only after the batch passed its last barrier. Inside `store`, `committed.acks` is the one constructor, and `committed` is reachable only through `planned.write`, then `written.sync`, then `durableData.linkRaws`, then `rawsLinked.linkManifests`. That is C2 as a type (per type-discipline). A one-line check (`grep -c 'Ack{' internal/store/*.go` equals 1) keeps a second literal from appearing.
- The committer's table, `map[layout.Key]*manifestBody`, is every source's manifest in memory, loaded once at `Open` (about 9k entries, a few MB). Disk manifests are its write-through persistence. The committer goroutine is the only writer; `Sources` reads it under an RWMutex for Welcome. This deletes the per-key lock map (`store.go:47-60`, never pruned) and the manifest read per append (`store.go:213`, `append.go:105`).
- The dirty set `changes` is owned by the store: the committer adds a batch's keys after its last barrier, and the cycle takes the whole set by swap (`TakeChanged`). Failed parses are carried by the index's `Lap`, never handed back. Two actors, two pieces of state, merged when the cycle reads them (per separate-before-serializing).
- `index.Lap` is the source list and checkpoints the boot listing read, kept in `listSources` order. Only `Index.Boot` makes one, so a dirty-set cycle cannot run before the full listing it depends on (per model-the-domain).

### Push stream

One bidirectional RPC, `mnemonic.push.v1.Push/Sync` (`internal/wire/pushpb/push.proto`):

- The client sends `Hello{machine}` once, then `Append{seq, harness, src_id, offset, head_hash, source_name, path, raw_end, body}`. These are a PUT's fields one for one (`docs/spec/wire.md:54-99`), so `ParseOp` serves both transports.
- The server sends `Welcome{sources, more, window}` pages of 4096 `SourceState`s (src_id, harness, size, head_hash, raw_end, updated_ms: exactly what the skip rules read, `push.go:94-106`). Then it sends one `Ack{seq, APPLIED|CONFLICT, state}` per Append, in send order.
- Rejected requests do not get a per-append error code. A bad segment, reserved name or malformed body ends the stream with a gRPC status (InvalidArgument, PermissionDenied). A correct client never sends one, and today's 400/403 are not retried either.
- Auth: the bearer token rides as `authorization` metadata. The native gRPC stream interceptor and HTTP `tokens.Require` both call `tokens.Check`; only that function parses the bearer header, checks the token and scopes, and attaches its Identity. Push requires `auth.Push`. Unknown tokens and missing scopes become Unauthenticated and PermissionDenied, respectively. The author still comes only from the token.
- Resume: the client keeps nothing on disk, as today (`internal/wire/wire.go:1-4`). On any stream end it backs off (`watch.go:201-209`), dials, reads Welcome and runs one full pass with the skip rules. Unacked appends are forgotten; Welcome says what landed. This deletes the manifest GET (`push.go:67-91`), the 6 h reconcile (`watch.go:26, 211-246`), `conflictRetry` and the 409 retry loop (`push.go:166-172`, `syncTail`'s fallback).
- Pipelining: `streamSource` sends a file's chunks without waiting for acks. The next chunk's offset is the previous one's end, which `syncSource` already assumes (`push.go:176`). `uplink.sources` is the server's state plus what is in flight; an APPLIED ack writes the server's state back only once no later append of that source is in flight. A CONFLICT ack writes it back and marks the path dirty, and every later in-flight append of that source conflicts too, because its offset no longer matches. Only the watcher goroutine touches `sources`; the ack reader goroutine only delivers.

### Flow control and backpressure

Three bounds, each with one owner:

1. Client window: Welcome advertises 64 appends and 96 MiB. `uplink.send` blocks while either is spent.
2. Server per-stream: the ticket channel has 64 slots. A client that ignores the window only stalls its own `Recv`.
3. Server global: `Submit` takes the compressed frame's bytes from a 256 MiB budget and blocks while it is spent. The bytes come back when the batch commits.

On a slow disk, batches take longer and grow to their caps. The budget fills, `Submit` blocks, `Recv` stops, and HTTP/2 flow control closes the stream window. The client's `send` then blocks in the watcher loop and file events queue in the notifier; an overflow already becomes a full pass (`lostEvents`, `watch.go:29`). The native transport stops reading beyond its fixed stream credit; it does not spool the rest of the request into an unbounded handler buffer. The server's state is re-read at the next Welcome. The compressed-frame budget is not a bound on all store memory (see below).

Message size: one chunk is up to 32 MiB (`push.go:219`), so the server sets `grpc.MaxRecvMsgSize(33 MiB)`. The chunk size cannot shrink without breaking C1.

Native transport limits (`internal/intake/intake.go`):

- `InitialWindowSize(64 KiB)` and `InitialConnWindowSize(256 KiB)` disable BDP estimation; `MaxConcurrentStreams(4)` bounds streams per connection. grpc-go's `inFlow.maybeAdjust` can temporarily grant a whole message of extra credit to finish an active read, so 64 KiB alone is not the peak receive-buffer bound. A 32 MiB Append still fits.
- For receive payloads, allow 33 MiB wire buffers plus 33 MiB decoded message per active stream, plus 64 KiB outstanding credit: four streams use at most 264.25 MiB of payload storage. Round to **272 MiB per connection** for the receive-transport accounting envelope, including frame/header bookkeeping. This is a receive-transport estimate, not a whole-process heap guarantee or an allocator/RSS cap.
- Headers are limited to 16 KiB and connection handshakes to 10 s. Keepalive enforcement is `MinTime: 20s, PermitWithoutStream: true`; server parameters are `Time: 45s, Timeout: 20s, MaxConnectionIdle: 5min` (idle means no active RPC). The client's 30 s pings are allowed.
- There is no connection cap (`internal/intake/intake.go:45-79`). Including the pre-acquire frame described below, allow **272 + 4 × 34 = 408 MiB per connection**, plus a shared **256 MiB** admitted-frame budget and **128 MiB** of overlap with a completed batch (`internal/store/commit.go:25-27, 558-564, 588-589, 614-615`). Thus the Push payload accounting envelope is **408N + 384 MiB**, or **2016 MiB for four connections**. Four connections is an operational expectation, not enforced admission control; the four-stream and message limits are enforced (`internal/intake/intake.go:31, 48-51`). This envelope retains the receive-transport estimate above; it is not a server-heap or RSS cap.

**Store memory caveat:** Each stream has one sequential receive/parse/Submit loop (`internal/intake/intake.go:252-278`): its current Append can retain up to 33 MiB of decoded payload during parsing/encoding or a blocked acquire, but `ParseLines` aliases the body (`internal/store/commit.go:36-41`), so this is the decoded-message term already included in the 272 MiB transport envelope, not another body copy. The same stream can additionally hold one encoded frame before admission (`commit.go:365-381`): round its length up to 34 MiB, since default zstd bounds 33 MiB input by 33 MiB + 3 × 265 + 14 bytes (klauspost/compress v1.19.1, `zstd/encoder.go:843-872`, `zstd/encoder_options.go:36-47`, `zstd/blockdec.go:40`). Hence the additional 4 × 34 MiB per connection. After Submit returns, requests retain only metadata, linesLen and the frame (`commit.go:204-210, 365-387`); the 64 queued tickets and one being acked contain no bodies or frames (`internal/intake/intake.go:175-178, 223-243`). The 1024-request queue and at-most-512 held requests share the 256 MiB frame-length budget, rather than adding another frame allowance (`commit.go:25-28, 246-247, 380-387, 538-564`). Clearing the completed prefix before re-slicing removes idle backing-array retention (`commit.go:529-532`); budget release still precedes that clear, so a just-completed Push batch can briefly retain up to 128 MiB while new submissions spend the returned credit (`commit.go:558-564, 588-589, 614-615`), included above. These are payload-length bounds: `bytes.Buffer` spare capacity and zstd workspace (`commit.go:1033-1045`), request/ticket metadata and growing source manifests (`commit.go:68-78, 110-112, 605-608, 698-707`), Welcome construction (`internal/intake/intake.go:217-220, 372-384`), index memory (`internal/index/handle.go:77-82`) and Go GC/allocator headroom remain outside them. Metadata strings are only checked for non-emptiness, not a small byte limit; server-owned Append also encodes before admission and charges at most 256 MiB per frame even for a larger frame (`commit.go:380-381, 426-448`). There is therefore **no finite configured worst-case total server-memory bound**, even after this retention fix. The 2016 MiB four-connection payload envelope is below **8192 MiB (MemoryMax=8G, `ops/systemd/mnemonic.service:20`)**, but does not establish that the server fits: admission/accounting for pre-acquire allocations, capacities, metadata and other writers remains follow-up work.

Listener proof (Mac, synthetic data, Oct 25 2026 PT; scratch tests, not committed): a Go overlay of b14f549's intake and the native intake adds the same application gate after Hello, so receive is stalled while an authenticated peer ignores the app window and never reads Welcome/acks. It attempts 1024 × 256 KiB Appends. Post-GC HeapAlloc at start / 8 s was **931560 / 272109576 bytes** on ServeHTTP (all 1024 sends completed), versus **884944 / 2914944 bytes** native (one send completed, unchanged at 4 s and 8 s). HeapInuse at 8 s was 273285120 versus 4055040 bytes. These are one run each (median equals the reported value; range is that single value), include the in-process client, and isolate transport buffering rather than store throughput. Native is limited by stream flow control; the old receiver accepts the entire offered flood. `/usr/bin/time -l` reported 342523904 / 230948864 bytes maximum RSS for the test commands, each below the 1 GiB Mac budget. A separate live-store flood stalled at 69 sends and 27154352 bytes HeapAlloc. An earlier fixture blocked response credit too, which also blocked net/http's window updates; it did not reproduce the unbounded buffer and is not the before/after proof.

The unchanged `internal/push` client sent and acked exactly 33554432 bytes through h2c on 127.0.0.1:8741, then reconnected and skipped the unchanged file with zero bytes sent. A 186 s idle `push.Watch` run used one connection, counted eight client PING frames (including setup probes), and zero GOAWAY frames, with another PING in each 31 s sample. Live-stream shutdown returned Unavailable; a stream stalled before Hello terminated through the bounded Stop fallback. These are local synthetic checks; Caddy's >10 min idle proof and the outside firewall check remain deployment gates.

### Group commit

There is one committer goroutine. It blocks for the first request, then takes everything already queued, up to 512 requests or 128 MiB of frames, with no timer. A lone append commits at once, and under load the batch grows to whatever arrived during the previous batch's barriers (classic group commit, self-tuning). Frames are compressed by `Submit`'s caller goroutine, in parallel, using the same `zstd.NewWriter` defaults with one Write and Close as `writeFrame` (`store.go:308-315`). The committer does only planning and IO.

The barrier primitive is per OS:

- Linux (prod): one `syncfs(2)` on the data dir's filesystem (`barrier_linux.go`, `unix.Syncfs` from golang.org/x/sys, which is already linked through grpc and gets promoted to direct). A batch costs 2 syncfs, or 3 when it holds a rewrite, however many sources it carries. Today N appends cost 3N fsyncs.
- Elsewhere (Mac tests, local mode): fsync each touched file, then each touched dir (`barrier_other.go`). `os.File.Sync` is F_FULLFSYNC on darwin.

A batch error is fatal except ENOSPC or EDQUOT from W1 allocation or file writes before B1: after rollback succeeds, only that batch's tickets fail with ResourceExhausted and the committer continues. Rollback errors, close/truncate/unlink errors, and all barrier or link errors stay fatal. A failed barrier is fatal even when its error is ENOSPC or EDQUOT. The committer stops, fails every waiting and later ticket, and the process exits non-zero; systemd restarts it (`ops/systemd/mnemonic.service:11`, `Restart=on-failure`) and `Open` reloads the truth from disk. After a failed syncfs/fsync the page-cache state is unknown and cannot be trusted (the PostgreSQL fsyncgate lesson), so the committer never retries a barrier.

### Dirty set

Server-side `wake()` (`store.go:40-45`, `server.go:48`) becomes `Changed()`, with the same cap-1 non-blocking semantics, plus `TakeChanged()`. Local mode's settle loop reads `Changed()` as it read `Wake` (`cmd/mnemonic/local.go:133, 218`).

## Proto

`internal/wire/pushpb/push.proto` (committed). Generated code is committed next to it. `gen.go` holds the pinned `go generate` line: protoc-gen-go v1.36.12 (go.mod's protobuf) and protoc-gen-go-grpc v1.6.0, each `go install`ed into a temp GOBIN, then protoc.

- Reader load: two generated files nobody reads, plus one `.proto` that is the spec. `docs/spec/wire.md` describes the stream by pointing at it.
- Toolchain cost: protoc is needed only to regenerate. The plugins are pinned and fetched by Go itself. A CI check regenerates and runs `git diff --exit-code`, so the .proto and the Go code cannot drift.
- Rejected: hand-written messages with protowire and a custom codec. They need no protoc, but they are a hand-synced copy of the .proto (design-red-flags: hand-synced list) and about 200 lines of marshal code that someone must read.

## Group-commit state machine

States, in order. Each transition is a method on the previous state's type, and only the last state makes Acks.

| State (type) | Entered by | On disk after it |
|---|---|---|
| queued (`request`) | `Submit` (frame encoded) | nothing |
| planned | `plan(table, reqs, now)`, pure | nothing |
| written | W1 `planned.write`: extends truncate raw to `before.ZstSize` and append frames; rewrites unlink the old manifest and write the new raw to a temp file; every after-manifest written to a temp file. No sync. | frames past zst_size, temp raws, temp manifests, unlinks, all possibly unsynced |
| durableData | B1 barrier | all of the above durable |
| rawsLinked | W2a rename each temp raw over its raw, then B2 (skipped when the batch has no rewrite) | new raws linked and durable, rewrites still without a manifest |
| committed | W2b rename each temp manifest, then B3 | every manifest durable: the commit point |
| (after) | table updated, keys added to changes, tickets resolved with `committed.acks()` | same |

The plan cuts the batch before a Replace of a key already planned in it, so that Replace starts the next batch; otherwise an Extend acked APPLIED in the same batch would name a frame that never reached disk. The plan folds several requests on one source into one `sourcePlan`, in queue order. The frames concatenate, so the raw holds the same bytes as sequential PUTs, and the manifest is written once with the final fields. `updated_ms` advances by `max(now, prev+1)` per applied request, as today; it is a run-to-run time field under C1.

The ordering rules are today's, made batch-wide:

- A manifest is renamed only after the frames it covers are durable (B1 before W2b), and the next append truncates to `zst_size` (`store.go:298`).
- A rewrite's old manifest is gone before its new raw is linked (B1 before W2a, the rule at `store.go:165-171`).
- A rewrite's raw is linked before its new manifest (B2 before W2b, `store.go:285-288` then `204`).

### Crash table

Nine crash points. A kill -9 keeps the page cache, so a write that returned survives. The "power loss" notes cover a lost cache.

| # | Crash during or after | On disk | How it converges |
|---|---|---|---|
| 1 | queued or planned | nothing new | Client redials; Welcome is the old state; it re-sends the same chunks. |
| 2 | W1, an extend's frames | bytes past `zst_size`, old manifest | Welcome shows the old size; the re-sent Extend truncates to `zst_size` and writes the same frame. The index reads only to `ZstSize` (`ingest.go:260-263`). |
| 3 | W1, a rewrite's unlink or temp raw | manifest gone or old; partial temp raw | `Open` deletes `.<name>.jsonl.zst.*` and `.<name>.json.*` temps. No manifest: Welcome has no entry and the client replaces from 0. Old manifest: the client re-decides as before. Same bytes either way. |
| 4 | W1, temp manifests | temp files | Deleted by `Open`; the real manifests are untouched (as 2 or 3). |
| 5 | after B1, during W2a | some rewrites' raws renamed, their manifests unlinked (durable) | No manifest: the client replaces from 0; the rename is repeated over whatever raw is there. |
| 6 | after B2, during W2b | some manifests renamed | A renamed manifest covers durable frames (B1) and a linked raw (B2), so it is consistent: Welcome reports it and the client sends nothing. An un-renamed one is case 2 or 5. |
| 7 | after W2b, before B3 | all renamed, not yet durable | kill -9: the same as committed, but only after `Open`'s boot barrier makes the renamed manifests durable before any Welcome or Boot listing reads them; without it a later power loss could take back a manifest a client already skipped. Power loss: each rename is atomic and either landed or not, so case 6. |
| 8 | after B3, before acks | durable | Acks are lost; Welcome shows the new state; `decide` returns noop. Server-owned retries hit the request key (`Duplicate`). |
| 9 | client kill -9 mid-stream | whatever batches committed; a gRPC message is never delivered in part | Requests already submitted still commit. On rerun, Welcome gives the sizes and the client cuts the rest from there. For a file that did not change, `firstChunk` lands on the same boundaries, so the frames are the same. |

ENOSPC/EDQUOT in W1 (before B1): truncate every attempted extend back to its before `zst_size`, remove all batch temps (including the partial failing temp), and drop from the table every key whose rewrite already unlinked its manifest. Do not restore that manifest with a new write on a full disk: no manifest is the valid state in row 3, and the client replaces from 0. Empty directories are harmless. Cleanup is idempotent; any cleanup error is fatal. A kill during cleanup leaves only the states in rows 2–4, which `Open` and client retry already recover. No barrier or link is reordered, and no failed ticket is acked.

Every row ends in the bytes an uninterrupted run writes. Rows 2 and 5 rely on the rerun re-sending, which the client always does because Welcome still shows the old state. Row 7 relies on `Open`'s boot barrier (one syncfs on Linux, per-file and per-dir fsync elsewhere) after the temp sweep and before `Open` returns. `Open` neither truncates raw tails nor deletes raw files that lack a manifest: the next Extend truncates (`store.go:298`). Rows 3 and 4 rely on `Open`'s temp sweep, which today's code lacks (today a crash leaves temp files behind forever).

## Dirty-set handoff

- Owner: the store. The committer adds a batch's keys to `changes` after B3, under `changes.mu`, then does a non-blocking send on `ready` (cap 1).
- Consumer: `server.Run` calls `lap.Next(st.TakeChanged(), opts)`. `TakeChanged` swaps the map under `changes.mu`.
- Racing appends: an add before the swap is in the taken set; an add after it is in the next set and has re-armed `ready`, so the loop runs another cycle. An add is never in neither set.
- Failed or deferred work: `Lap.carry` holds the keys whose parse failed. Version-stale sources come from the cached checkpoints (`versionStale`), so the sweep needs no dirty entry. A cycle's work is `changed ∪ carry ∪ sweep`.
- Crash: the set is memory only. After a restart, `Boot` does the full listing and checks every source's manifest against its checkpoint (`ingestSource`'s `extends`, `ingest.go:330-333`). A key that was committed but not yet ingested before the crash is found there. Keys are added only after B3, so the index never parses bytes a crash could take back.
- 5-min and reconcile cycles: the timer cycle stays, because hot-to-cold settling is driven by time (`settleSource`, `server.go:83-85`; `due` in `publish`, `ingest.go:780-781`). It runs `Next` with whatever changed, usually nothing, and lists nothing. Its log reason becomes `settle`, since it no longer reconciles anything. The sweep loop (`server.go:100`) is unchanged.
- Premise: after boot, raw/ changes only through the committer. Archive files are written by nothing since import-ch retired (`internal/index/archive.go:22`). Copying files into raw/ by hand, for example an R2 restore, needs a restart.

What `Next` removes from a put cycle, per unchanged source (about 9k):

- the manifest glob and read (`ingest.go:110-126`);
- the archive glob and stat (`archive.go:50-75`);
- the checkpoint read (`ingest.go:167-178`);
- `ingestSource`'s open, stat, 64 KB window hash and magic read (`ingest.go:251-263, 286-297`);
- for each archive file, `ingestArchive`'s window hash and `archiveSetFor` glob (`archive.go:274-290`).

What stays O(all sources) or O(all parts), and the dirty set does not touch:

- `publish`'s loop building `cks` and the sources rows (`ingest.go:760-794`). It is in memory, but it reads every archive file's and member's checkpoint from disk (`ingest.go:771-785`).
- `settleSet` and `planParts` over every checkpoint (`ingest.go:1103, 1014`).
- `readManifest` and `prevPartsMap`, which read every part's base (`build.go:279-296`).
- `rematerialize` (`archive.go:818`) and `retireSettled` (`archive.go:721`).
- Marshalling the CURRENT with about 9k source rows, and the compare against the old one (`ingest.go:842-845`).
- `gcParts` and `gcDeltas` directory listings (`ingest.go:1151`, `delta.go:447`).
- `gcLogs`, which lists `srcDir`, about 27k entries (`ingest.go:1163-1185`).
- `Refresh`, which reopens every part through `OpenDB` whenever the CURRENT changed (`handle.go:68`, `db.go:57-66`).

Until the profile lands, which of these limits the 8 s cycle is unknown. The dirty set removes the per-source raw reads, which are about 9k × 64 KB ≈ 576 MB of page-cache reads plus sha256 per cycle (inferred from the code, not measured). The rest is the next lever if the profile points there.

## Gateway (C8)

- Two listeners in one process: `listen = "0.0.0.0:8740"` serves HTTP/1.1 (MCP and HTTP OTLP), and required `push_listen = "0.0.0.0:8741"` serves native `grpc.Server.Serve`. No implicit port default; missing, malformed or identical push addresses fail configuration load. The HTTP h2c configuration is removed. The push listener is firewalled exactly like 8740 by `loyal-agent-memory`; main must verify public 8741 times out from outside at deploy.
- Shutdown closes the push listener first, cancels live sessions with Unavailable, and runs GracefulStop with a 10 s shutdown deadline and Stop fallback for peers stalled before Hello or on responses. HTTP shutdown and the cycle-loop join finish before the deferred committer Close commits its queued writes and releases the index.
- Caddy routes `/mnemonic.push.v1.Push/*` to `h2c://host.docker.internal:8741` with `flush_interval -1`. Everything else stays on the current HTTP/1.1 upstream.
- The `request_body max_size 256MB` block must move into the non-gRPC route. Caddy caps a request's whole body, and a stream's body is every byte the client sends for its whole life, so the cap would cut the stream after 256 MB.
- OTLP over gRPC: left for later. No known client asks for it. When one does, it is two `Register*ServiceServer` calls on the same `grpc.Server` over the receiver's existing decode path, behind the same route rule.

## Tradeoffs accepted

- We accept that a syncfs waits on every dirty page in the filesystem, including the index's writes, in exchange for O(batches) barriers with no new on-disk format. Steady-state index writes are small (logs 120 KB and parts 437 KB per put cycle in the quoted journal line). A parser-bump sweep or a rebuild writes far more, so ack latency rises then.
- We accept a second listener and required config key to obtain native gRPC flow control. Auth policy remains one shared token check; no HTTP/2 handler transport is used for Push.
- We accept that a failed barrier exits the process, in exchange for never acking from a page cache we cannot trust.
- We accept two boot listings, the store's table and the index's `Boot`, in exchange for keeping the index independent of the store's internals.
- We accept that the dirty set's premise (only the committer writes raw/) turns hand edits of raw/ into "restart required".
- We accept a larger client binary (grpc linked; unmeasured) in exchange for flow control, status codes and a spec file.

## Alternatives considered

- Write-ahead log for group commit. Each batch appends one record (frames plus after-manifests) to a journal, takes one fdatasync and acks; raw files and manifests are applied without fsync and checkpointed later; boot replays. It reaches 1 barrier per batch and is not coupled to index writeback. It lost because it adds an on-disk format, a replay path and a checkpoint cycle, and raw files still need a later fsync. Its interface is the same as ours, but the complexity it hides is larger and new. If syncfs latency under sweeps turns out to matter, this is the fallback, behind the same `Submit` and `Ack`.
- Per-file fsync with folding. Fsync each touched raw and manifest once per batch and share dir syncs. This is portable and has no syncfs coupling, but the fsync count is 2 × distinct sources per batch. A first push or a catch-up after sleep touches thousands of distinct sources, so it stays O(N) where it matters.
- A timer batching window (for example 5 ms). It adds latency at low load and needs tuning; drain-on-idle needs neither.
- A separate native gRPC listener (:8741) was initially rejected for configuration and auth duplication. That decision is reversed: grpc-go v1.84.0 `internal/transport/handler_server.go:435-453` drains req.Body into an unbounded recvBuffer independently of application Recv, bypassing backpressure through net/http. Native transport with fixed windows fixes that buffer, and shared `Tokens.Check` avoids a second token policy.
- Dirty set as manifest values instead of keys. The cycle would read zero manifests, but the index would depend on the store's table type. Keys plus a re-read of a handful of files keeps the boundary narrow.
- A persisted dirty set, to skip the boot listing. Boot is once; the listing is its safety net.

## Open questions and risks

- How long does a syncfs take during a sweep on prod's disk? The box run should report ack p50 and p99 with a sweep running. If p99 passes a few seconds, do we switch to the WAL?
- Are klauspost zstd frames identical when encoded into a buffer instead of streamed into the file, at any GOMAXPROCS? The C1 gate answers this; nothing here proves it.
- Two clients pushing under the same machine name race on the same sources exactly as they do today. Should Hello refuse a second live stream for the same (author, machine)?
- Does `deploy/verify.sh` (not found in this repo) grep the cycle reason `reconcile`? Renaming it to `settle` would need that check updated.
- Should the 64-append / 96 MiB window be configurable in server.toml, or fixed until measured?
- Is it acceptable that a 401 retries forever on backoff, as today, rather than exiting?

## Lane decomposition (C9)

Blocking scaffold, first and alone: merge this sketch (proto, generated code, `go.mod` grpc direct, the panicking signatures). Every lane then builds against it.

| Lane | Owns | Implements (seeded contract) | Gate |
|---|---|---|---|
| L1 store | `internal/store/*`, `cmd/mnemonic/local.go`; the one-token `*store.Store` -> `*store.Committer` rename in `internal/otlp/receiver.go`, `internal/index/controls.go`, `internal/index/q_annotate.go` (by `gofmt -r`) | `Open`, `Close`, `Submit`, `Append`, `Sources`, `Changed`, `TakeChanged`, `Ticket.Wait`, `Ack`, `ParseOp`, `ParseLines`, `NewClientSource`, the batch states, both barriers; deletes the old `Store` and its HTTP handlers | `go test ./internal/store` with one table test: three requests on one source in one batch give the literal frame bytes and manifest of three sequential PUTs at 83b0042 |
| L2 intake + server | `internal/intake/*`, `internal/server/server.go`, `cmd/mnemonic/server.go`, `ops/Caddyfile` | `Mount`, `service.Sync`, `parseAppend`, `ackOf`, `welcome` (the sketch's `adapter.ServeHTTP` — servePut and serveManifest — was deleted on the PUT census instead of built), h2c `Protocols`, the `Run` loop over `Boot`/`Next`/`Changed` | `go test ./internal/intake`: an in-process h2c server; Welcome then three Appends return three APPLIED acks with literal sizes |
| L3 index | `internal/index/lap.go`, `internal/index/ingest.go` | `Boot`, `Lap.Next`, `Lap.patch`; `parseSources` over a work subset; `publish` writing settled checkpoints back | `go test ./internal/index`, the arena parity 126/126, and a scratch count: `Next` with one changed key reads 1 manifest |
| L4 client | `internal/push/*`, `cmd/mnemonic/main.go`, `cmd/mnemonic/init.go` | `dial`, `send`, `acks`, `applyAck`, `ended`, `close`, `streamSource`; deletes `fetchManifest`, `putSource`, the 409 loop, `reconcileEvery`, `conflictRetry` | `go test ./internal/push` against L2's in-process server (after merging L1 and L2) |
| L5 spec + checks | `docs/spec/wire.md`, `AGENTS.md`, `scripts/` checks | the stream section of the spec; a check that `Ack{` appears once in `internal/store`; a check that regenerating pushpb leaves no diff | the two checks fail on a planted second literal and on a hand-edited `push.pb.go` |

L4 depends on L1 and L2 at test time, not at build time, because the sketch compiles. The foreman merges L1, L2 and L3 first, then L4 and L5. The integration gates (C1 equivalence on the box, the C3 crash script at kill points 2, 5 and 8 on the server and mid-chunk on the client, the C6 strace syncfs count, and the C7 manifest-read count) run after all five merge.

## Next implementation step

L1's `plan` and `planned.write` against the three-sequential-PUTs table test, because every other lane's acks and every C1 byte depend on them.

## Note: rejected, and least sure

- Rejected: the WAL (new on-disk format and replay for one fewer barrier), per-file fsync (stays O(N) on a first push), a timer window (latency at low load), hand-written protowire (hand-synced with the .proto), a persisted or value-carrying dirty set.
- Reversed rejection: the second native gRPC listener is now required because ServeHTTP has an unbounded recvBuffer (`handler_server.go:435-453`). Both transports call one token check.
- Least sure: syncfs latency while the index writes heavily. The design's O(batches) claim holds by construction, but whether the per-batch barrier is cheap on prod's disk during a sweep is unmeasured. It is the one thing that would send this design to the WAL alternative.

# Uploader candidate B: preserve files, batch filesystem barriers

Architecture sketch, based on `83b0042`. Started 2026-10-25 19:29:47 PDT; deadline 20:44:47 PDT. No implementation or deployment is claimed.

## Problem

One long-lived upload RPC should replace manifest polling; one server writer should batch durability; an explicit changed-source handoff should remove discovery from ordinary index cycles. Frames and final manifests must remain byte-identical to the HTTP path, except timestamps. That excludes a new raw layout and merging compression frames. Ordinary per-file fsync cannot deliver constant barrier count across N distinct files. This candidate therefore uses Linux `syncfs` over the raw filesystem, with the existing manifest-as-commit-record model, rather than adding a WAL. The supplied 8.282 s cycle is one journal sample (no run series or limiter evidence), not evidence that manifest reads dominate it; the profile remains a prerequisite for a performance claim.

## Usage (caller's view)

Client watcher call site (pseudocode):

```go
client, err := push.OpenStream(ctx, cfg, red)
if err != nil { return err }
defer client.Close()
// Existing notifier/quiet window passes changed paths to this Sink.
stats, err := client.Sync(ctx, changedPaths)
```

`OpenStream` obtains the initial server snapshot; `Sync` hides chunk planning, pipelining, acks and reconnect. The watcher continues watching before its initial scan. Remote watch deletes manifest GET, the six-hour timer and conflict retry. Overflow or watcher rearm failure triggers a local rescan; disconnect triggers both a fresh RPC snapshot and a local rescan. Local `WatchSink` retains its present behavior.

Server transport call site (inside `upload.Handler`, pseudocode):

```go
lease, err := committer.OpenMachine(ctx, identity, hello.Machine)
defer lease.Close()
initial, err := lease.Snapshot()
sendSnapshotThenReady(initial)
mutation, err := store.AcceptUpload(identity, input)
pending, err := lease.Submit(ctx, mutation)
// Receive and submit more work while an independent sender waits.
durable, err := pending.Wait(ctx)
sendAck(sequence, durable)
```

Cycle call site:

```go
live, err := ix.OpenLive(ctx, committer)
// Boot, settled PUT wake, five-minute maintenance and version sweeps:
stats, err := live.Cycle(ctx, index.IngestOptions{Quiet: settleSource})
```

These are illustrative call sites, not error-handling implementations. Production must test every error and never dereference a failed Open result.

## Shape

`Committer` owns durable source positions, writer leases, a bounded admission queue and `Changes`. `MachineLease` owns a consistent connect snapshot and admission for one `(authenticated author, machine)`. `Upload` and `ServerMutation` are separate validated domains; only a machine lease admits an upload. `Pending` is an unresolved outcome. Sealed `Durable` is the only successful result; its private receipt is produced only by `syncedBatch.complete`. `LiveIngest` owns the full source catalog used for publication and chooses a sparse parse subset. The client owns speculative offsets separately from acknowledged offsets.

Per boundary-discipline, protobuf and HTTP query data become sealed upload variants at the edge; auth is `Tokens.Context` plus `Identity.Can(auth.Push)`, never an author in a message. Per encode-lessons-in-structure, private phase types make receipt creation unavailable before the last barrier. Per separate-before-serializing, clients prepare/redact independently; only admitted disk mutations share a writer. Per subtract-before-you-add, there is no durable request-id ledger, WAL or second raw representation.

The public surface hides recovery, barriers, queueing and byte credits. `Store()` exists only to migrate the established MCP/OTLP callers without exposing the scheduler to them; it is the facade for this same writer. Callers do not coordinate flush phases. `OpenCommitted` pins an inode plus its committed compressed prefix under the same per-source publication lock as writes: index parsing cannot accidentally combine an old manifest with replacement bytes. It releases that lock immediately after opening, not after parsing. Appends never truncate below a published prefix; replacement uses rename, so an open old inode remains valid.

Module map: `internal/store/uploader_sketch.go` owns durability and handoff; `internal/push/uploader_sketch.go` owns remote client behavior; `internal/upload/uploader_sketch.go` is the server boundary; `internal/index/uploader_sketch.go` owns cycle selection. Existing runtime callers remain unchanged in this candidate.

## Proto

The complete schema is [internal/upload/v1/uploader.proto](../../internal/upload/v1/uploader.proto). `Uploader.Push` is bidi: Hello; server SourceState messages then Ready; client Start/Data/End per logical append; server Ack per durable append. No application author, request-id dedup key, polling RPC or conflict reply. Sequence numbers are per-connection correlation only. Fragmentation preserves the old PUT boundary and never creates additional zstd frames.

Generation choice: standard protoc-generated protobuf and grpc-go bindings, committed by the blocking scaffold step. Pin protoc 35.1 (installed here), protoc-gen-go v1.36.12 and protoc-gen-go-grpc v1.6.0; use source-relative generation. No custom codec or handwritten protobuf reflection. Candidate contracts deliberately do not reference absent generated bindings, so the sketch compiles without generation or dependency changes. The exact version-checking `go:generate` directive is committed in `internal/upload/v1/generate_sketch.go`. Both Go generator versions are present in the local module cache; the grpc generator release is [upstream v1.6.0](https://chromium.googlesource.com/external/github.com/grpc/grpc-go/+/refs/tags/cmd/protoc-gen-go-grpc/v1.6.0). Install each with `go install <generator>@<version>` into a private GOBIN, put it on PATH, then `go generate ./internal/upload/v1`. The scaffold commits `uploader.pb.go` and `uploader_grpc.pb.go` and promotes the existing grpc v1.84.0 dependency to direct. Normal consumers only need Go; protoc is a schema-editor tool, not a build prerequisite. No `go mod tidy` is needed for this candidate.

### Session and flow control

The server first authenticates exactly one bearer value in RPC metadata, checks push scope, validates Hello version 1 and machine, and acquires `(author,machine)`. It sends a stable snapshot, sorted by `(harness,src_id)`, from its durable in-memory catalog, ending with Ready. Empty inventory is Ready alone. The client waits for Ready before sending; the key is `(harness,src_id)`, not src_id alone. Another active stream for that key receives AlreadyExists; release waits for admitted predecessor writes to finish before a reconnect can snapshot. Different machines remain independent. Existing HTTP PUT obtains a transient lease; it cannot race an active stream. There is no last-writer-wins takeover.

Client Start/Data/End messages describe exactly one old PUT body. Start has a strictly increasing sequence and declares its byte count; data fragments are at most 1 MiB; End requires exact count and a trailing newline (or the existing positive-offset empty backfill). One body is assembled at a time per RPC; up to eight completed operations can await acks, across files or from one file. The server checks the client's expected position against its **projected admission state**, including earlier queued operations, so pipelining does not create false conflicts. It never admits an append whose predecessor failed.

Defaults are eight unacked operations and 64 MiB unacked logical bytes per RPC; the server enforces both. A single legacy chunk larger than the byte window is permitted only when the window is empty, and remains the only outstanding operation until ack. This preserves the long-line case without pretending that 32 MiB is a hard maximum. Large bodies are spooled to temporary redacted files with bounded transfer buffers; fragment size is not a new logical chunk boundary. Global admission is capped at 128 operations, 64 MiB normal queued logical bytes, with the same exclusive oversized rule. The transport reserves a receiving/spool slot before reading the next body; the committer reserves its queue credits at Submit before consuming the completed spool. A blocked Submit stops that RPC from receiving another body. Validate a completed seekable spool's exact length and final newline, then rewind it; validation must not consume the body's only reader. Limit simultaneously receiving bodies to 16; idle streams retain no body buffer. Spool capacity is checked against actual available disk; quota/ENOSPC is ResourceExhausted, never a shortened append. RAM bounds cover the transport queues, not the existing client's whole-file redaction implementation.

One goroutine receives, one sends acks; the writer never waits for network Send. Slow disk fills admission credits, stops application Recv, and lets bounded gRPC/HTTP2 receive windows stop the client sender. The ack reader continues draining while the sender blocks. Configure finite connection/stream windows (initially 1 MiB each): because grpc.Server.ServeHTTP uses Go's HTTP/2 transport, set `http.Server.HTTP2.MaxReceiveBufferPerConnection/PerStream`, not grpc.Server's native-transport window options. On the native grpc-go client use the static window options to disable adaptive growth; gRPC Send returning is transport progress, never durability. Use a 2 MiB receive-message ceiling to allow the 1 MiB fragment plus protobuf overhead. An oversized metadata field is rejected before body admission.

Acks are in admission/sequence order for one RPC; each names its own committed redacted size and raw_end, even when the durable manifest is already at a later prefix. Mark a file stamp synced only after all its appends have acked; an event arriving during upload remains dirty for a subsequent pass. Do not replan an in-flight body from a changing file. On a file rewrite, drain or abandon the current RPC, then plan a replacement from the stable next read; never mix two local observations into one logical chunk.

On EOF, timeout, FailedPrecondition (position/head mismatch), Unavailable or ResourceExhausted, close the entire RPC, discard speculative positions, and reconnect with the existing 5 s to 1 min capped backoff plus jitter. Only the new snapshot decides what to resend. Bad syntax or permission is a surfaced configuration/data error, not an endless reconnect loop. An ack lost in transit is harmless: reconnect learns the later durable prefix. One-shot push waits for all acks and exits; watch retains the RPC. No HTTP fallback and no on-stream conflict correction. Configure Go HTTP2 SendPingTimeout=60 s, PingTimeout=20 s and WriteByteTimeout=60 s for the backend connection; the client uses matching keepalive with PermitWithoutStream=false (the upload RPC is active). Caddy terminates HTTP/2 on each leg, so these are hop-level checks; transport tests must also prove frontend disconnect cancels the backend RPC. AlreadyExists from a still-draining lease retries with the same capped backoff. Use finite dead-peer detection; the server must release a disconnected lease and drain its admitted operations before granting it again.

### Byte equivalence

Reuse `firstChunk`, `decide`, redaction, head hashing and raw_end calculation; move transport out of those decisions. `internal/push/push.go:221-229` deliberately emits a whole first line when it exceeds maxChunk. Do not “fix” that in this migration. Preserve the baseline's raw_end/mtime shortcuts, including their limitations for same-size rewrites, so this work does not quietly change which files upload.

Every logical append gets a fresh `zstd.NewWriter` with exactly the old defaults, then Close exactly once. No Flush, EncodeAll substitution, frame-size hint, compressor reuse or frame coalescing. Streaming fragmented input through Write preserves the encoder's input-length-based block fill: at the pinned klauspost version, `writeBlocks` and `writeJobs` accumulate to fixed block/job sizes and Close alone finalizes. This is a source-based argument, not measured byte parity; the corpus/long-line/frame-boundary gate must prove it before shipping. Stage compressed output separately, then concatenate the complete **frames** in source order. Frame concatenation preserves bytes; recompressing their combined bodies does not.

Use the existing `manifestBody` JSON field order, newline, zst_size, and omitted Applied semantics. A normal client never sets requestKey. Replacement resets metadata and Applied as today; append ignores newly supplied source_name/path as today; empty backfill changes raw_end only when positive and leaves updated_ms unchanged. Existing server-owned appends retain current/previous-day Applied checks, including same-batch duplicates and midnight ordering. The actor evaluates those against projected manifests, not stale disk reads. It submits duplicate/no-op outcomes through the same receipt-producing phase, with no second success constructor.

### Caddy and process wiring

Keep `server.toml.listen` and one server process. Enable HTTP/1 and unencrypted HTTP/2 on the Go 1.26 `http.Server.Protocols`; mount the grpc.Server HTTP handler on `/mnemonic.upload.v1.Uploader/`, leaving MCP and HTTP OTLP on their existing routes. External TLS remains Caddy's responsibility; h2c is loopback-only. The client verifies normal TLS certificates and sends its bearer only through TLS, except an explicitly configured loopback development endpoint.

In Caddy, route that gRPC service path to `reverse_proxy h2c://127.0.0.1:8740` with no request_buffers/response_buffers and no finite stream lifetime. Leave flush_interval at its default: unknown-length gRPC responses already flush immediately; setting -1 can suppress backend cancellation on frontend disconnect. Keep the 256 MB request-body cap **only on non-stream routes**. Applying it to the whole long-lived RPC would eventually kill an otherwise healthy watch client after 256 MB total. Caddy documents [h2c upstreams and streaming controls](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy); its [request body limit](https://caddyserver.com/docs/caddyfile/directives/request_body) applies to the entire request. Deploy server/Caddy before clients; old HTTP clients continue to work throughout.

Leave OTLP/gRPC for a later change. The listener becomes capable of it, but registering OTLP services would also require auth, limits, cancellation and response/error parity; that behavior is not free. Shutdown cancels streams, stops new admission, drains or aborts the current batch without early receipts, then closes the committer and index. HTTP shutdown must explicitly terminate active bidi RPCs rather than wait forever.

## Group-commit state machine

`collectingBatch -> stagedBatch -> invalidatedBatch -> installedBatch -> syncedBatch -> receipts`.

Form a batch on an 8 ms fixed deadline from the first complete admitted append, or 128 appends / 64 MiB body, whichever first. Never slide the deadline. Apply each source's operations in FIFO order, emitting one frame per old PUT, but write its final manifest only once. Split the batch before a replacement of a key already present in it: otherwise an earlier append could be acked after its bytes were discarded. A replacement may be followed by appends of that new generation in the same batch. No ack is released while a batch remains tentative. Cross-source batches are not atomic transactions; each manifest independently commits a prefix.

1. Stage frames and final manifest temp contents; replacement frames go in fresh temp inodes, appends truncate only uncommitted tails before adding frames.
2. Unlink old manifests for replacements. Barrier A durably flushes all staged contents, new ancestor directories and those invalidations.
3. Rename replacement raw files. Barrier B makes their entries durable; omit B when none exist.
4. Rename manifests. Barrier C makes all final manifests and directory entries durable.
5. Publish positions to the in-memory catalog, mark changed keys, then resolve per-append receipts. All intermediate prefixes named by receipts are durable in the final source.

Linux uses `syncfs` on a persistent descriptor on the raw filesystem at each barrier. Require one filesystem for the raw tree and reject nested mounts; Linux >=5.8 error reporting is required. A successful Linux syncfs provides filesystem-wide fsync-equivalent completion ([Linux manual](https://www.man7.org/linux/man-pages/man2/syncfs.2.html)). A batch costs two barriers for appends or three with replacements, independent of touched source count. This must be measured as `fsync + fdatasync + syncfs`, not hidden by counting fsync alone. It also flushes unrelated dirty files on that filesystem; that is the primary risk.

On macOS, `os.File.Sync` each touched regular file, then sync each affected directory (including newly created ancestors, child before parent) in the same phase order, propagating every failure. `F_FULLFSYNC` on regular files is additionally required for a power-loss qualification; ordinary process-kill tests use the baseline fsync contract. Do not use Darwin sync(), whose completion contract is insufficient. There is no claimed cross-inode O(batches) bound on macOS. Local mode keeps its existing path. If C6 intends the constant bound on every OS, that interpretation conflicts with the unchanged multi-file layout and portable per-file fsync; this candidate satisfies the production Linux bound and makes the limitation explicit.

### Recovery and crash table

Use one persistent filesystem descriptor so syncfs writeback-error tracking is not reset by reopening it per batch. Any write, close, rename, unlink or barrier error poisons the committer: fail unresolved requests, refuse snapshots and further writes, and require recovery/restart. Do not continue with an in-memory guess after a partial publish. Reads already pinned by the index remain safe. A missing manifest is an uncommitted source; a malformed manifest, raw shorter than zst_size, or impossible offsets fails recovery loudly rather than pretending the source is empty.

Before listening, take the existing exclusive server writer lock **before** recovery or LoadControls (current server.New takes it later). OpenCommitter recovers raw, then load controls, open/refresh the index, seed LiveIngest, and finally expose handlers. Enumerate raw once, remove abandoned dot temps, truncate tails after each manifest's zst_size, and discard raw files with no manifest. These cleanup operations are themselves idempotent. Sync recovered files/namespace before exposing inventory, then seed all committed source keys as dirty. A manifest visible after process death but not yet synced becomes durable at this boot barrier before the server advertises its offset. No ack or snapshot relies merely on post-crash visibility.

The table partitions every mutating interval; “during” includes a crash immediately before or after each constituent write/rename/sync. Prefix means a sequence of complete original logical chunks. Disk states across different keys may differ; the protocol does not promise cross-source atomicity.

| # | Crash interval | Possible disk state | Boot/reconnect convergence |
|---|---|---|---|
| 1 | Receiving fragments, before End/admission | Only incomplete spool; canonical old pair intact | Drop spool, snapshot old prefix, resend identical chunk. |
| 2 | Admitted/queued, before stage | Old files only | Snapshot old prefix; resend. No durable receipt exists. |
| 3 | Writing append frames or replacement/manifest temps, before invalidation | Old manifest; append may have arbitrary tail; replacement raw still old | Truncate append to old zst_size; remove temps; resend from old prefix. |
| 4 | Unlinking replacement manifests, before/during barrier A | Some old manifests survive, others absent; old replacement raw still installed | Surviving pairs resume; absent manifests cause orphan removal and replay from zero. No old manifest can refer to new replacement raw yet. |
| 5 | After A, before raw installation | Append frame tails and temps durable; replacement manifests durably absent | Append resumes from old manifest; replacements replay from zero. |
| 6 | During replacement raw renames / barrier B | Old or new replacement raw at each path, but no replacement manifest | Remove orphan raw, replay from zero. Append tails truncate to old manifest. |
| 7 | After B, before manifest rename | All final raw bytes and names durable; old append manifests / absent replacement manifests | As above: old append prefix or zero, then identical replay. |
| 8 | During manifest rename loop | Some new manifests visible, others old/absent; every new manifest references durable raw | Boot keeps each visible valid committed prefix, trims extra tail, syncs it; replay only remaining chunks. |
| 9 | During barrier C | Same mixture under power loss; after process kill visible new manifests may all remain | Same per-key rule, with boot barrier before snapshot. Still no pre-C receipt. |
| 10 | After C, before catalog update/dirty marking | Complete durable final pairs | Boot catalog and all-dirty seed recover the lost in-memory publication. Client snapshot skips committed chunks. |
| 11 | During catalog/dirty update, before waking waiters | Durable final pairs; in-memory publication partial | Kill discards memory; boot reconstructs. No network ack yet. |
| 12 | While sending some acks, after all acks, or during cleanup | Durable pairs; some clients uncertain; dot temps may remain | Reconnect final snapshot; never resend already-covered chunks. Cleanup is idempotent. |

Thus **12 server crash intervals** cover admission, all three barriers, publication and delivery. Also script three client kill points: mid-fragment, End sent with ack unread, and between consecutive acknowledged chunks. A fourth useful point is midway through the initial snapshot. The deterministic rerun oracle is the original immutable input tree, the same redactor/version/chunking, and the same starting store; it compares all frame and manifest files, normalizing only updated_ms. File mutation between attempts is a new input, not a meaningful byte-identical crash oracle.

Replace-at-zero intentionally uses today's “manifest absent => replay” recovery rather than retaining both generations or introducing a WAL. Newly admitted writes may complete after client cancellation; Pending.Wait returning a context error means unknown outcome, not rollback. The next machine lease waits for those operations before it constructs the snapshot. Checkpoint/index files are derived and may lag an ack; boot replay is their recovery mechanism.

The type boundary does not pretend Go has linear types: zero Upload/ServerMutation, nil Durable, and repeated phase use must be rejected. Phase constructors and the concrete receipt stay private; the transport's ack encoder accepts `Durable`, never `SourceState`, and rejects nil. Only `syncedBatch.complete` constructs receipts, after the store's barrier functions return successfully. Mutation inputs cannot manufacture an ack by filling exported fields.

## Dirty-set handoff

Store-owned `Changes.pending[key] = generation` is marked only after durability, before ack. `Take` swaps the map under a mutex. Concurrent appends enter the new map; finishing a cycle never deletes from that new map. Failed work max-merges back. The wake channel is only a hint. A crash loses this memory, and the boot inventory seeds every surviving committed source again.

Only boot lists raw manifests; connect snapshots use the store catalog. Every writer in serve mode goes through the committer: stream, HTTP adapter, OTLP, annotations and controls. `Committer.Store()` supplies the compatibility Store facade for existing MCP/OTLP callers. `Store.Append` keeps its API but waits on the server-owned mutation path and maps the durable result back to its existing return type. Raw_end-only changes also mark the catalog dirty (parsing can return Unchanged). No caller merely invokes wake without recording a key. Cycle parses dirty, retry and parser-version sweep keys. Publication still receives the complete catalog: calling current `ingest` with only dirty sources would drop CURRENT members and let GC remove live logs. Five-minute cycles retry failures and maintain parts; they never rediscover raw files. Archive inventory is loaded at boot and treated as immutable until restart. Offline import/replacement/deletion requires stopping serve and restarting after the change; a periodic raw scan is not hidden behind “reconcile.” Restart is also how a parser-version change becomes visible. Boot reads parser/checkpoint versions for every known source and seeds the existing bounded sweep; failed versions keep the current failSweep suppression, and a newer dirty generation clears that source's suppression. Missing checkpoints seed parse work even for sources that otherwise look unchanged.

Race example: Take detaches `{A:7}`; A is opened at durable generation 7; an append commits generation 8 and records `{A:8}` in the new map; cycle completion drops only its detached map. Generation 8 remains. If OpenCommitted instead sees generation 8, the next cycle may harmlessly revisit A. No lost update depends on timing a channel send. Cycle failure retries detached generations with max, never overwriting a newer pending value. ChangeBatch.Retry accepts keys from that detached batch; selected sweep/retry keys outside it remain in LiveIngest.retry instead of inventing a store generation. Partial parse failure retries failed keys; publication/Refresh failure retries the entire detached batch plus any selected sweep/retry keys, even when their checkpoints already advanced. Five-minute cycles retry ordinary failures; sweep poison sources remain reported without a tight retry loop. A cycle is serial under the existing index lock.

Server boot obtains `Inventory` once before opening listeners. Incremental cycles refresh metadata only for taken keys through the committer's catalog and open raw only for selected parsing. Therefore post-boot raw manifest reads can be zero rather than merely O(dirty); disk reads are replaced by the writer's already-validated metadata. The index must take metadata and the raw FD from the **same** OpenCommitted result and apply its compressed bound with an io.SectionReader; the prior catalog row alone is not a safe raw snapshot.

Removed: `listSources` glob/manifest unmarshalling for unchanged sources and their `ingestSource` open/windowSHA reads. Not removed: `readCkpts` for all known sources; `publish` scans all sources and archive member checkpoints; settleSet/componentsOf/planParts, prior-part maps, rematerialize, buildParts planning, retirement and GC still traverse global state. The current `archiveSetFor` (`archive.go:173`) additionally globs/stats/hashes all ch files, both from ingestArchive and restreamMembers (`:290`, `:875`). The serve path must pass its boot-known archive path inventory through those calls and remove their repeated glob, otherwise “only at boot” would be false. Digest/stat/window work and winner rebuilding over that entire known archive set remain O(all archive files/rows) when invoked; this is not the separate archive CPU optimization. These costs may dominate; this design does not claim a fully O(dirty) cycle.

### HTTP adapter and local mode

`LegacyPUT` expires **2026-11-25** (also in its code comment). Keep the old manifest GET for old clients until the same deletion; it returns the durable catalog rather than reading files. The adapter preserves old query validation, wire.Source responses and 409 mapping for an actual offset mismatch, but calls lease.Submit/Wait and can never call appendZst/writeManifest directly. A machine already owned by a live stream gets an explicit busy response (423); running old and new daemons for the same machine is an operator error. The new client has no 409 code. Census by authenticated author/machine/token identifier and protocol, never token value; after every known client moves, remove the GET/PUT routes and adapter together. Reaching expiry without a complete census is an owner-visible migration failure, not an automatic deletion that strands clients.

Local mode keeps direct file ingest, WatchSink and its current caller API; it never instantiates a gRPC client or the server batch scheduler. Refactor its existing synchronous store helpers into the same private frame/manifest phase operations with per-file barriers and immediate single-operation completion, so local server-owned annotations retain their timing while serve has exactly one committer. This is code reuse of persistence rules, not a second asynchronous write path or a new local architecture. Local's periodic source discovery remains local-only; deleting the remote six-hour reconcile must not delete that behavior accidentally.

## Synthesis decision

Reserved for the orchestrator. This is an independent candidate, not a synthesized decision.

## Tradeoffs accepted

- We accept filesystem-wide barriers and their interference with index IO in exchange for truly shared durability without a WAL or changed raw bytes.
- We accept one server writer scheduling batches, including its compression CPU and large-append head-of-line cost, in exchange for explicit publication order and one recovery model. Only the measured profile justifies later bounded parallel compression; group commit does not promise to remove every CPU limiter.
- We accept unchanged global index planning in exchange for a narrow, auditable removal of discovery and unchanged-source raw reads.
- We accept generated binding files in exchange for ordinary protobuf tooling and no custom serialization knowledge.

## Alternatives considered

- A shared redo WAL hides append fsync well, but C2 still requires the actual frame, manifest and directories durable before ack; it adds recovery state without removing those barriers.
- Per-source leaders plus per-file fsync are portable and isolate latency, but cost O(distinct touched sources per batch), not O(batches) for cross-file uploads.
- Feeding only dirty sources to existing ingest is a small call site but an invalid abstraction: publish and GC interpret the argument as the complete retained universe.
- Periodic full discovery is simple operationally but violates boot-only listing and preserves the cost this feature targets.

## Open questions and risks

- Can the Linux deployment guarantee a supported local filesystem, kernel >=5.8 and a single raw mount, with mount changes forbidden while running? Unsupported topology must fail startup, not silently weaken barriers.
- Will preserving the current client's raw_end/mtime shortcuts be acceptable until a separately scoped same-size-rewrite fix, rather than changing the parity oracle in this migration?
- Will the measured index IO on the same filesystem make syncfs materially worse than per-file syncing despite the reduced barrier count?
- Does the owner require the C6 asymptotic bound on macOS too, despite its production-only motivation?
- Can archives and raw files be contractually immutable outside this server while it runs, with offline edits requiring restart?

## Next implementation step

Land a blocking scaffold that generates bindings, seeds the signatures and wires no runtime behavior; then fill the disjoint lane contracts below.

## Lane decomposition

One blocking **scaffold step** lands first: commit generated bindings with the pinned directive; promote grpc to direct without changing its version; settle all exported signatures in these sketches; seed the `Store` committer attachment/construction hook used by serve and the synchronous local compatibility wrapper; seed the `Server` live-ingest field; add compile-only call sites guarded from execution until their owners fill them. It owns go.mod/go.sum and generated files so no parallel lane runs tidy or regenerates bindings. Both normal and server builds must pass. It does not enable a panic stub in a running service.

After scaffold, these **five Sonnet lanes** have disjoint files. They implement the seeded contracts, not alternate wrappers. Paths below are prospective implementation ownership, not files this candidate modifies.

| Lane | Exclusive files | Seeded contract | Gate before integration |
|---|---|---|---|
| 1. Store | `internal/store/*.go`; new store crash hooks only here | `OpenCommitter`, `AcceptUpload`, `AcceptServerAppend`, `Committer.Submit/Store/Inventory/OpenMachine/OpenCommitted/Changes/Close`, `MachineLease.Submit/Snapshot/Close`, `Pending.Wait`, sealed `Durable`, `Changes.Take/Wake`, `ChangeBatch.Keys/Retry`, `LegacyPUT`; existing `Store.Append` compatibility | Both builds; literal frame/manifest fixture parity; inject each of the 12 phase failures, prove no receipt before the successful barrier; verify same-batch duplicate/midnight/backfill rules. |
| 2. Server transport | `internal/upload/uploader_sketch.go` and new files directly under `internal/upload/` (not `v1/`); `internal/server/server.go`; `cmd/mnemonic/server.go`; `ops/Caddyfile` | `upload.Handler(*store.Committer, auth.Tokens) http.Handler`; existing server.New/Run/Close call the scaffold's committer/live fields | Both builds; metadata scopes and machine lease isolation; fragmented/oversized bodies; slow disk and unread acks bounded; Caddy h2c RPC survives aggregate 256 MB while HTTP routes retain their cap. |
| 3. Client | `internal/push/*.go`; `cmd/mnemonic/main.go` | `OpenStream`, `StreamClient.Sync/Close`; existing Sink remains; migrate Run/Watch callers and delete HTTP-only helpers | CGO-free client build; exact existing chunk boundaries including >32 MiB first line; grep remote path for zero manifest GET/6 h reconcile/409 retries; notifier overflow and reconnect recovery; local WatchSink behavior retained. |
| 4. Index | `internal/index/uploader_sketch.go`, `ingest.go`, `handle.go`, `kind.go`, `archive.go` | `Index.OpenLive(ctx,*store.Committer)`, `LiveIngest.Cycle(ctx,IngestOptions)`; raw helpers accept io.ReaderAt; archive inventory threaded through publication | Both builds; boot-only source listing counter, zero unchanged raw opens on a PUT cycle, race A:7/A:8 and partial-publication retry; unchanged CURRENT members/logs survive a one-key cycle; parity 126/126 on the authorized box. |
| 5. Evidence and migration spec | `docs/spec/wire.md`; `scripts/uploader/` new fixtures, driver and crash/parity counters; evidence document under `docs/design/uploader-validation.md` | External black-box drivers consume CLI/RPC and lane 1's seeded crash-hook names `queued`, `stage`, `invalidate`, `after-a`, `install`, `after-b`, `manifest`, `barrier-c`, `catalog`, `dirty`; transport lane 2 seeds `receive`/`ack`; no production code ownership | Same input and baseline revision, per-file SHA comparisons, three client kill points plus all server intervals, counters for every sync primitive and listing/open calls; report median/range/run-count/limiter/errors under the box lock. |

Each lane also runs repository-required formatting/vet/build gates under the shared Mac lock; feature tests follow the main packet's policy, with literal fixture/crash evidence rather than adding trivial unit tests. No real corpus run on the Mac. Lane 5 can build the driver while 1–4 implement, then execute it after integration on an explicitly authorized box; this candidate neither runs it nor grants deployment permission. Foreman wires census and final adapter removal after real client rollout; only main deploys. Heap-bound repair, trailer enforcement and dependency-doc cleanup are separate standing work, not silently added to these file packets.

## Evidence and red-flag screen

Verified locally against `83b0042`: real client is `internal/push/push.go`, not the packet's stale `cmd/mnemonic/push.go` path; default command wiring is `cmd/mnemonic/main.go:43`. Store frame/manifest/dir syncs are `store.go:319`, `:361`, `:371`; syncDir currently discards errors (`:377-384`), which the new barrier path must correct. The usual nonempty append is three syncs; replacement and backfill have different counts, so the benchmark must stratify operations. Source discovery and unchanged-byte checks are `ingest.go:109`, `:167`, `:306`; complete-universe publication begins at `:747`. Both `ingest.go` and its server/handle callers were read in full. GROUND.md was read in full; no profile output or production access was used.

Screened against the supplied design red flags: no public protobuf re-export; no caller-managed flush phases; one serve writer; no WAL mirror; no client polling and stream dual path; retained complete publication universe is required index state, not a hand-synced list (new keys enter through the writer catalog). Lease and receipt boundaries add exclusion/durability policy instead of forwarding identical calls. The deliberately global publication pass and Linux filesystem coupling are visible costs, not hidden behind a shallow “incremental” name.

### Candidate verification, 2026-10-25 20:04 PDT

All final checks below exited 0 under `lockf -k /Users/user/xstock-scratch/build.lock nice -n 15 env GOMAXPROCS=4 GOFLAGS=-p=2`. The formatting check uses an equivalent shell conditional so an empty gofmt listing exits 0. No tests, corpus processing, deployment or benchmark were run.

| Command | Outcome |
|---|---|
| `gofmt -l .` with nonempty-output failure | PASS, no paths |
| `CGO_ENABLED=0 go build ./...` | PASS |
| `CGO_ENABLED=0 go build -tags server ./...` | PASS |
| `go vet ./...` | PASS |
| `CGO_ENABLED=0 go build -o /dev/null ./cmd/mnemonic` | PASS |
| `CGO_ENABLED=0 go build -tags server -o /dev/null ./cmd/mnemonic` | PASS |
| `protoc -I internal/upload/v1 --descriptor_set_out=/dev/null internal/upload/v1/uploader.proto` | PASS, schema parses |
| `go generate -n ./internal/upload/v1` | PASS, directive expands; no bindings generated |

`git diff --check` also passed. Early sandboxed builds exited 1 because the shared Go cache was inaccessible (nice also reported a permission error); rerunning the authorized locked checks with sandbox escalation passed. An intermediate queued format check exited 1 after concurrent local sketch edits; the final formatted checks above passed. No go.mod/go.sum change and no tidy. Final verdict is **type-check-only**, not test-verified or live-verified. Generated bindings are deliberately left to the blocking scaffold; domain sketches compile without them. No runtime caller invokes a stub.

### Sketch symbol locations

This index covers every Go declaration added by the candidate, including private phase/marker declarations. Paths are relative to the repository; no existing runtime declaration was changed.

- `internal/store/uploader_sketch.go:17` SourceState; `internal/store/uploader_sketch.go:25` StoredSource; `internal/store/uploader_sketch.go:33` UploadInput; `internal/store/uploader_sketch.go:41` Upload; `internal/store/uploader_sketch.go:42` ServerMutation; `internal/store/uploader_sketch.go:43` mutation; `internal/store/uploader_sketch.go:44` uploadChange; `internal/store/uploader_sketch.go:48` payload; `internal/store/uploader_sketch.go:52` position; `internal/store/uploader_sketch.go:56` replaceUpload; `internal/store/uploader_sketch.go:62` appendUpload; `internal/store/uploader_sketch.go:68` backfillUpload; `internal/store/uploader_sketch.go:73` serverChange; `internal/store/uploader_sketch.go:79` replaceUpload.mutation; `internal/store/uploader_sketch.go:80` replaceUpload.uploadChange; `internal/store/uploader_sketch.go:81` appendUpload.mutation; `internal/store/uploader_sketch.go:82` appendUpload.uploadChange; `internal/store/uploader_sketch.go:83` backfillUpload.mutation; `internal/store/uploader_sketch.go:84` backfillUpload.uploadChange; `internal/store/uploader_sketch.go:85` serverChange.mutation; `internal/store/uploader_sketch.go:87` AcceptUpload; `internal/store/uploader_sketch.go:93` AcceptServerAppend; `internal/store/uploader_sketch.go:99` Durable; `internal/store/uploader_sketch.go:104` receipt; `internal/store/uploader_sketch.go:110` receipt.State; `internal/store/uploader_sketch.go:111` receipt.Duplicate; `internal/store/uploader_sketch.go:112` receipt.durable; `internal/store/uploader_sketch.go:114` Pending; `internal/store/uploader_sketch.go:115` commitResult; `internal/store/uploader_sketch.go:120` Pending.Wait; `internal/store/uploader_sketch.go:124` Committer; `internal/store/uploader_sketch.go:130` admission; `internal/store/uploader_sketch.go:135` OpenCommitter; `internal/store/uploader_sketch.go:141` Committer.Submit; `internal/store/uploader_sketch.go:147` Committer.Close; `internal/store/uploader_sketch.go:148` Committer.Inventory; `internal/store/uploader_sketch.go:149` Committer.Store; `internal/store/uploader_sketch.go:150` Committer.Changes; `internal/store/uploader_sketch.go:156` MachineLease; `internal/store/uploader_sketch.go:161` Committer.OpenMachine; `internal/store/uploader_sketch.go:166` MachineLease.Snapshot; `internal/store/uploader_sketch.go:167` MachineLease.Submit; `internal/store/uploader_sketch.go:168` MachineLease.Close; `internal/store/uploader_sketch.go:172` CommittedSource; `internal/store/uploader_sketch.go:178` Committer.OpenCommitted; `internal/store/uploader_sketch.go:179` CommittedSource.State; `internal/store/uploader_sketch.go:180` CommittedSource.CompressedEnd; `internal/store/uploader_sketch.go:181` CommittedSource.ReadAt; `internal/store/uploader_sketch.go:185` CommittedSource.Close; `internal/store/uploader_sketch.go:189` collectingBatch; `internal/store/uploader_sketch.go:190` stagedSource; `internal/store/uploader_sketch.go:196` stagedBatch; `internal/store/uploader_sketch.go:200` invalidatedBatch; `internal/store/uploader_sketch.go:201` installedBatch; `internal/store/uploader_sketch.go:202` syncedBatch; `internal/store/uploader_sketch.go:204` collectingBatch.stage; `internal/store/uploader_sketch.go:213` stagedBatch.invalidateAndSync; `internal/store/uploader_sketch.go:218` invalidatedBatch.installAndSync; `internal/store/uploader_sketch.go:223` installedBatch.publishAndSync; `internal/store/uploader_sketch.go:230` syncedBatch.complete; `internal/store/uploader_sketch.go:238` Changes; `internal/store/uploader_sketch.go:244` ChangeBatch; `internal/store/uploader_sketch.go:249` Changes.Wake; `internal/store/uploader_sketch.go:250` Changes.Take; `internal/store/uploader_sketch.go:255` ChangeBatch.Keys; `internal/store/uploader_sketch.go:256` ChangeBatch.Retry; `internal/store/uploader_sketch.go:265` Committer.LegacyPUT

- `internal/store/uploader_barrier_linux_sketch.go:8` syncStoreBarrier

- `internal/store/uploader_barrier_darwin_sketch.go:8` syncStoreBarrier

- `internal/push/uploader_sketch.go:14` StreamClient; `internal/push/uploader_sketch.go:19` uploadSession; `internal/push/uploader_sketch.go:20` connectingUpload; `internal/push/uploader_sketch.go:21` snapshottingUpload; `internal/push/uploader_sketch.go:24` readyUpload; `internal/push/uploader_sketch.go:29` disconnectedUpload; `internal/push/uploader_sketch.go:33` uploadSource; `internal/push/uploader_sketch.go:34` remotePosition; `internal/push/uploader_sketch.go:38` sourceProgress; `internal/push/uploader_sketch.go:39` uploadFlight; `internal/push/uploader_sketch.go:47` connectingUpload.uploadSession; `internal/push/uploader_sketch.go:48` snapshottingUpload.uploadSession; `internal/push/uploader_sketch.go:49` readyUpload.uploadSession; `internal/push/uploader_sketch.go:50` disconnectedUpload.uploadSession; `internal/push/uploader_sketch.go:52` OpenStream; `internal/push/uploader_sketch.go:57` StreamClient.Sync; `internal/push/uploader_sketch.go:63` StreamClient.Close

- `internal/index/uploader_sketch.go:15` LiveIngest; `internal/index/uploader_sketch.go:22` Index.OpenLive; `internal/index/uploader_sketch.go:27` LiveIngest.Cycle

- `internal/upload/uploader_sketch.go:17` receivePhase; `internal/upload/uploader_sketch.go:18` snapshotting; `internal/upload/uploader_sketch.go:19` ready; `internal/upload/uploader_sketch.go:23` receiving; `internal/upload/uploader_sketch.go:30` snapshotting.receivePhase; `internal/upload/uploader_sketch.go:31` ready.receivePhase; `internal/upload/uploader_sketch.go:32` receiving.receivePhase; `internal/upload/uploader_sketch.go:36` ackSender; `internal/upload/uploader_sketch.go:38` ackSender.send; `internal/upload/uploader_sketch.go:46` Handler

The proto service/messages are in `internal/upload/v1/uploader.proto:7`; the pinned generator directive is `internal/upload/v1/generate_sketch.go:7`.

Final note: rejected WAL duplication, per-source-only batching and dirty-only publication for the reasons above. Least sure: whether filesystem-wide flushing competes too strongly with the index writer to be a performance win.

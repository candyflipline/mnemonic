# Uploader candidate B: preserve files, batch filesystem barriers

Architecture sketch, based on `83b0042`. Started 2026-10-25 19:29:47 PDT; deadline 20:44:47 PDT. No implementation or deployment is claimed.

## Problem

One long-lived upload RPC should replace manifest polling; one server writer should batch durability; an explicit changed-source handoff should remove discovery from ordinary index cycles. Frames and final manifests must remain byte-identical to the HTTP path, except timestamps. That excludes a new raw layout and merging compression frames. Ordinary per-file fsync cannot deliver constant barrier count across N distinct files. This candidate therefore uses Linux `syncfs` over the raw filesystem, with the existing manifest-as-commit-record model, rather than adding a WAL. The supplied 8.282 s cycle is an observation, not evidence that manifest reads dominate it; the profile remains a prerequisite for a performance claim.

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
sendSnapshotThenReady(lease.Snapshot())
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

`Committer` owns durable source positions, writer leases, a bounded admission queue and `Changes`. `MachineLease` owns a consistent connect snapshot and admission for one `(authenticated author, machine)`. `Mutation` is validated input. `Pending` is an unresolved outcome. Sealed `Durable` is the only successful result; its private receipt is produced only by `syncedBatch.complete`. `LiveIngest` owns the full source catalog used for publication and chooses a sparse parse subset. The client owns speculative offsets separately from acknowledged offsets.

Per boundary-discipline, protobuf and HTTP query data become `Mutation` at the edge; auth is `Tokens.Context` plus `Identity.Can(auth.Push)`, never an author in a message. Per encode-lessons-in-structure, private phase types make receipt creation unavailable before the last barrier. Per separate-before-serializing, clients prepare/redact independently; only admitted disk mutations share a writer. Per subtract-before-you-add, there is no durable request-id ledger, WAL or second raw representation.

The public surface hides recovery, barriers, queueing and byte credits. Callers do not coordinate flush phases. `OpenCommitted` pins an inode plus its committed compressed prefix under the same per-source publication lock as writes: index parsing cannot accidentally combine an old manifest with replacement bytes. It releases that lock immediately after opening, not after parsing. Appends never truncate below a published prefix; replacement uses rename, so an open old inode remains valid.

Module map: `internal/store/uploader_sketch.go` owns durability and handoff; `internal/push/uploader_sketch.go` owns remote client behavior; `internal/upload/uploader_sketch.go` is the server boundary; `internal/index/uploader_sketch.go` owns cycle selection. Existing runtime callers remain unchanged in this candidate.

## Proto

The complete schema is [internal/upload/v1/uploader.proto](../../internal/upload/v1/uploader.proto). `Uploader.Push` is bidi: Hello; server SourceState messages then Ready; client Start/Data/End per logical append; server Ack per durable append. No application author, request-id dedup key, polling RPC or conflict reply. Sequence numbers are per-connection correlation only. Fragmentation preserves the old PUT boundary and never creates additional zstd frames.

Generation choice: standard protoc-generated protobuf and grpc-go bindings, committed by the blocking scaffold step. Pin protoc 35.1 (installed here), protoc-gen-go v1.36.12 and protoc-gen-go-grpc v1.6.0; use source-relative generation. No custom codec or handwritten protobuf reflection. Candidate contracts deliberately do not reference absent generated bindings, so the sketch compiles without generation or dependency changes. The final generation directive and verified plugin availability are detailed in the next revision of this document.

## Group-commit state machine

`collectingBatch -> stagedBatch -> invalidatedBatch -> installedBatch -> syncedBatch -> receipts`.

Form a batch on an 8 ms fixed deadline from the first complete admitted append, or 128 appends / 64 MiB body, whichever first. Never slide the deadline. Apply each source's operations in FIFO order, emitting one frame per old PUT, but write its final manifest only once. No ack is released while a batch remains tentative. Cross-source batches are not atomic transactions; each manifest independently commits a prefix.

1. Stage frames and final manifest temp contents; replacement frames go in fresh temp inodes, appends truncate only uncommitted tails before adding frames.
2. Unlink old manifests for replacements. Barrier A durably flushes all staged contents, new ancestor directories and those invalidations.
3. Rename replacement raw files. Barrier B makes their entries durable; omit B when none exist.
4. Rename manifests. Barrier C makes all final manifests and directory entries durable.
5. Publish positions to the in-memory catalog, mark changed keys, then resolve per-append receipts. All intermediate prefixes named by receipts are durable in the final source.

Linux uses `syncfs` on a persistent descriptor on the raw filesystem at each barrier. Require one filesystem for the raw tree and reject nested mounts; Linux >=5.8 error reporting is required. A successful Linux syncfs provides filesystem-wide fsync-equivalent completion ([Linux manual](https://www.man7.org/linux/man-pages/man2/syncfs.2.html)). A batch costs two barriers for appends or three with replacements, independent of touched source count. This must be measured as `fsync + fdatasync + syncfs`, not hidden by counting fsync alone. It also flushes unrelated dirty files on that filesystem; that is the primary risk.

On macOS, sync each touched regular file and directory explicitly in the same phase order, propagating every failure. There is no claimed cross-inode O(batches) bound on macOS. Local mode keeps its existing path. If C6 intends the constant bound on every OS, that interpretation conflicts with the unchanged multi-file layout and portable per-file fsync; this candidate satisfies the production Linux bound and makes the limitation explicit.

Crash table and failure semantics are being expanded before the final checkpoint; no implementation may start from this partial section.

## Dirty-set handoff

Store-owned `Changes.pending[key] = generation` is marked only after durability, before ack. `Take` swaps the map under a mutex. Concurrent appends enter the new map; finishing a cycle never deletes from that new map. Failed work max-merges back. The wake channel is only a hint. A crash loses this memory, and the boot inventory seeds every surviving committed source again.

Only boot lists raw manifests; connect snapshots use the store catalog. Cycle parses dirty, retry and parser-version sweep keys. Publication still receives the complete catalog: calling current `ingest` with only dirty sources would drop CURRENT members and let GC remove live logs. Five-minute cycles retry failures and maintain parts; they never rediscover raw files. Archive inventory is loaded at boot and treated as immutable until restart.

Removed: `listSources` glob/manifest unmarshalling for unchanged sources and their `ingestSource` open/windowSHA reads. Not removed: `readCkpts` for all known sources; `publish` scans all sources and archive member checkpoints; settleSet/componentsOf/planParts, prior-part maps, rematerialize, buildParts planning, retirement and GC still traverse global state. ArchiveSetFor can still scan all archive files when archive parse/rematerialization invokes it. These costs may dominate; this design does not claim a fully O(dirty) cycle.

## Synthesis decision

Reserved for the orchestrator. This is an independent candidate, not a synthesized decision.

## Tradeoffs accepted

- We accept filesystem-wide barriers and their interference with index IO in exchange for truly shared durability without a WAL or changed raw bytes.
- We accept one server writer scheduling batches in exchange for explicit publication order and one recovery model.
- We accept unchanged global index planning in exchange for a narrow, auditable removal of discovery and unchanged-source raw reads.
- We accept generated binding files in exchange for ordinary protobuf tooling and no custom serialization knowledge.

## Alternatives considered

- A shared redo WAL hides append fsync well, but C2 still requires the actual frame, manifest and directories durable before ack; it adds recovery state without removing those barriers.
- Per-source leaders plus per-file fsync are portable and isolate latency, but cost O(distinct touched sources per batch), not O(batches) for cross-file uploads.
- Feeding only dirty sources to existing ingest is a small call site but an invalid abstraction: publish and GC interpret the argument as the complete retained universe.
- Periodic full discovery is simple operationally but violates boot-only listing and preserves the cost this feature targets.

## Open questions and risks

- Will the measured index IO on the same filesystem make syncfs materially worse than per-file syncing despite the reduced barrier count?
- Does the owner require the C6 asymptotic bound on macOS too, despite its production-only motivation?
- Can archives and raw files be contractually immutable outside this server while it runs, with offline edits requiring restart?

## Next implementation step

Land a blocking scaffold that generates bindings, seeds the signatures and wires no runtime behavior; then fill the disjoint lane contracts below.

## Lane decomposition

Pending final file ownership table. Maximum five lanes; no candidate implementation is included.

Final note: rejected WAL duplication, per-source-only batching and dirty-only publication for the reasons above. Least sure: whether filesystem-wide flushing competes too strongly with the index writer to be a performance win.

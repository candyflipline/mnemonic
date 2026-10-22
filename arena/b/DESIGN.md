# Candidate b: immutable Go segments

## Format and publication

Raw remains authoritative. A source checkpoint stores its compressed boundary,
64 KiB trailing-window SHA-256, parser state, maximum event version, source
identity, and immutable segment names. Appends read only new compressed frames.
The parser version and private format version are recorded in the manifest.

A source transaction creates one or more immutable directories:
- `meta.zst`: gob metadata with previews, text hash, document length, type flags,
  tool input, provenance, and heavy-block locators; zstd level 3.
- `heavy`: independent zstd frames containing gob arrays of text, payload and
  token usage. Each frame has a four-byte compressed-length prefix. Targets are
  256 KiB uncompressed or 128 events; an oversized event occupies its own block.
- `terms`: MNTERM02, term count, lexically sorted front-coded ASCII tokens and
  delta-varint (local document, frequency) postings. Lengths are uvarints.
- `manifest.json`: atomic publication of source checkpoints and segment names.

A segment targets 8 MiB of normalized text/payload/token bytes or 32,768 events,
plus one event. Parsing stays serial; two writers overlap indexing, compression
and I/O, with eight queued events each. Publication groups up to 16 sources.
Staged files/directories use ordinary fsync. The manifest uses File.Sync before
rename and parent-directory Sync afterward: on Darwin these are full device-cache
flush barriers. Parser state advances with every referenced segment, never alone. Monotone append
versions preserve the existing lake's replacement rule across process restarts.
Equal-version winners prefer live sources, then newer ingest stamps, then source
file order. Compaction preserves those stamps; format v4 reparses older stores.

## Query memory and structures

The query process streams metadata into process-local file-backed mmap arenas.
Fixed rows/UID bytes and large variable strings use separate mappings to reduce
cold-page traffic. A fixed 2 KiB per-segment term filter avoids negative lookups.
Temporary arena files are unlinked immediately; mappings own their lifetime.
Their Go pointers are process-local, never serialized as a reusable file format.
All string data and embedded pointers refer to mapped storage, except segment
pointers held independently by the Store. Snapshot replacement closes mappings
only after the old call finishes. This representation needs review before reuse
as a general library: returned strings borrow the snapshot's lifetime.

The following per-event structures are mapped, not retained on the Go heap:
metadata and document lengths; sorted UID lookup; ordered session vectors;
turn counts, ordinals and terminals; session time/completion values; local/live
ID maps; reversed-path postings; score accumulators and posting scratch space.
Term bytes and heavy frames map their immutable files directly. A sparse term
fence every 16 terms lives in the arena. No per-segment file descriptor stays open.

The heap retains source manifests, segment/session lookup entries, codecs, and a
payload cache capped at 8 MiB of decoded wire bytes. Sample measurements after
GC: startup heap 0.73 MB, warm heap 11.9 MB, versus 32.0/43.2 MB before this change.
The metadata/index view maps about 32 MB here, in addition to mapped heavy files.
This unlinked scratch backing consumes disk while batch is alive; it is additional
to the directory-byte score. Virtual mappings are not live Go heap. After 300 ms
idle, synchronous page invalidation and GC release reclaimable resident pages;
queries fault them back on demand. Checker idle RSS fell from 98.0 to 26-43 MB.
The largest public cases after idle took 47 ms for search and 59 ms for open;
responses remained identical. Warm checker timings do not include that cold cost.

- `search_sessions`: exact global BM25 from segment postings and mapped columns.
  Terms sum lexically; the original two sort stages, tie order, pre-dedupe limit,
  score normalization, duplicate equivalence and prose remain unchanged.
- `file_attention`: segment-local reversed path/command-token indexes, followed
  by exact original predicates. Arrays, malformed input and unusual shell tails
  have conservative candidate fallbacks. Project scope remains closed.
- `open`: binary UID lookup, ordered session vectors, turn metadata, lazy heavy
  frames and the original navigation/content/cursor logic.
- `list_sessions`: mapped per-session rollups, sorted by update time and ID;
  binary cursor seeks with the original filters and labels. Built once per
  snapshot; p50 fell from 3,165 to 21-33 microseconds.
- `get_ingest_status`: source manifest counters and original status derivation.

## Ingest profile and parser scope

The first CPU profile attributed 52% of samples to repeated parser filesystem
probes and 8% to heavy-block Seek calls. The lane-local `normalize` copy preserves
parser logic, adds bounded per-ingest filesystem lookup caches, and runs the
original literal golden and parser-version checks. A cache reset test covers
filesystem changes between ingests. Only files inside this lane are modified.
Compressed input and heavy output are buffered; offsets are tracked directly.
The ASCII scanner preserves lowercasing, length limits and token frequencies.
All five tools pass public, hidden, incremental and three-kill recovery checks.
Clean ingest improved from 6.12 to 2.16-3.49 seconds. macOS full flushes and serialized
parse/write work were additional wall-time costs after removing repeated probes.

## Recovery and freshness

A local advisory lock serializes ingest. Kills leave only unpublished temporary
or complete segments. Restart reads the last durable manifest, resumes its raw
boundary, and reclaims unreachable files. Unchanged ingest leaves the manifest
unchanged. Queries pin mapped files, so unlinking old segment names is safe.
The batch process checks the manifest before every call and loads a new snapshot
when it changes. There is no raw-tail query overlay or background daemon here.

Source replacement/shrinkage, changed checkpoint windows, and parser-version
changes reparse raw. A parser upgrade resets checkpoints, then publishes groups of source
transactions with the new version. A kill resumes that sweep; queries during it
can see a partially rebuilt corpus. A production upgrade should build and switch
an entire generation if uninterrupted whole-corpus visibility is required.
Missing record timestamps use one deterministic far-future value, as required by
CONTRACT-NOTES, preserving order across clean, incremental and crash runs.

## 500x scale: 8.8 million events, not 100x

Idle heap grows with sources, sessions and segments, plus the fixed payload
budget; per-event arrays grow in mapped storage. Startup still performs global
UID/session sorts and reconstructs the view, O(N log N) work and O(N) mapped
space. Mapped working-set RSS depends on access and OS reclamation; the sample
heap result is not a measured 500x RSS guarantee. Persisted portable columns and
incremental view publication would remove the full startup/reload rebuild.
At unchanged sample density, 500x implies roughly 16 GB of derived files plus
16 GB of runtime view backing; that is a disk extrapolation, not an RSS forecast.

Merges combine eight consecutive intervals of one source at the same tier.
Total input files are capped at 2 MiB; after three promotions a segment never
merges again. This bounds per-event derived writes to four including the initial
write. Heavy frames copy without decompression; metadata offsets and posting IDs
rebase. A one-byte `tier` file records promotions. Old files remain until manifest
publication and normal garbage collection, so pinned readers remain valid.
Larger intervals remain separate, and no merge crosses source boundaries.
The intentional duplicate heavy payload is additional to raw storage.
Each publication group (up to 16 sources) rewrites the global manifest: O(S)
metadata bytes per group and O(S squared / 16) over a clean many-source build.
A checkpoint journal remains necessary at production scale. Merge planning
inspects only the changed source's segments, but is not yet an indexed planner.

Peak ingest memory is O(two segment budgets + codecs + in-flight records +
source manifest + decoded merge postings). The 2 MiB merge cap bounds compressed
input, not its decoded footprint. Each writer queues at most eight events. The sample peak
is about 188 MB. The required 900 MiB Go soft limit remains enabled in runs;
it is not a hard RSS ceiling. The parser accepts unbounded single raw lines, so
there is no unconditional sub-1-GB bound for arbitrary inputs. Production needs
an explicit oversized-record policy and a measured bound before adoption.

## Rejected alternatives and remaining limits

SQLite/another SQL engine adds dependencies for five fixed tools. An FST loses
to front coding on the supplied digit-heavy corpus. Block-max MAXSCORE and
128-document bitpacking remain deferred; exact exhaustive scoring is simpler.
Raw-only locators cannot guarantee bounded reads from whole-source zstd frames,
so heavy payloads are intentionally duplicated. No production-scale corpus run
was performed. The local parser fork must track upstream fixes and version bumps.

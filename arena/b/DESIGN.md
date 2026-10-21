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

A segment targets 8 MiB of normalized text/payload/token bytes, plus one event.
Every file is fsynced, followed by directory sync and rename. Only then is a
new manifest fsynced, renamed and its parent synced. Parser state advances with
all segments for that source, never independently of them. Monotone append
versions preserve the existing lake's replacement rule across process restarts.

## Query memory and structures

The query process streams metadata into process-local file-backed mmap arenas.
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
GC: startup heap 0.49 MB, warm heap 11.7 MB, versus 32.0/43.2 MB before this change.
The metadata/index view maps about 30 MB here, in addition to mapped heavy files.
Virtual mappings and reclaimable file pages are not the same as live Go heap.

- `search_sessions`: exact global BM25 from segment postings and mapped columns.
  Terms sum lexically; the original two sort stages, tie order, pre-dedupe limit,
  score normalization, duplicate equivalence and prose remain unchanged.
- `file_attention`: segment-local reversed path/command-token indexes, followed
  by exact original predicates. Arrays, malformed input and unusual shell tails
  have conservative candidate fallbacks. Project scope remains closed.
- `open`: binary UID lookup, ordered session vectors, turn metadata, lazy heavy
  frames and the original navigation/content/cursor logic.
- `list_sessions`: session vectors and original aggregate/filter/cursor logic.
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

## Recovery and freshness

A local advisory lock serializes ingest. Kills leave only unpublished temporary
or complete segments. Restart reads the last durable manifest, resumes its raw
boundary, and reclaims unreachable files. Unchanged ingest leaves the manifest
unchanged. Queries pin mapped files, so unlinking old segment names is safe.
The batch process checks the manifest before every call and loads a new snapshot
when it changes. There is no raw-tail query overlay or background daemon here.

Source replacement/shrinkage, changed checkpoint windows, and parser-version
changes reparse raw. A parser upgrade resets checkpoints, then publishes source
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

Current merge policy is append-only: no live segment is rewritten or compacted.
Segment write amplification is one derived write per new event, including the
intentional duplicate heavy payload; it is not one byte per raw byte. Each source
publication rewrites the global manifest, O(S) metadata bytes per changed source
and O(S squared) over a clean many-source build. This is a production limitation.
A production size-tiered merge with fanout 8 and four bounded levels would add at
most four full rewrites per event (five including initial write), plus manifest
metadata. A checkpoint journal should replace repeated whole-manifest writes.
Neither that merge policy nor the journal is implemented in this prototype.

Peak ingest memory is O(segment budget + largest normalized record + source
manifest), independent of total event count for fixed source count. The sample
peak is about 135 MB. The required 900 MiB Go soft limit remains enabled in runs;
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

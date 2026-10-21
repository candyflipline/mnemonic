# arena/a: a DuckDB-free store

A single static binary, `a ingest|batch -root R -store S`. It is pure Go
(CGO_ENABLED=0), and its only non-std dependency is klauspost/compress for zstd.
The parsers are the repo's own (`internal/normalize`). The tool code is
`internal/query` copied in, with its SQL replaced by reads of the index
(`q_*.go`).

## On-disk formats

```
S/src/<author__machine__harness__srcid>.ckpt    JSON, tmp+fsync+rename: the source's commit point
S/src/<key>.<gen>.light | .heavy                append-only zstd frame logs
S/parts/<fingerprint>/                          one immutable index part (below)
S/CURRENT                                       JSON {parts, sources}, tmp+fsync+rename: the index commit point
```

- **Frame log.** u32 length, u32 count, then zstd-3 records (128 KB light,
  256 KB heavy frames). Light (`row.go`) holds every column but payload_json
  and token_usage_json, which are heavy: read only by `open` and session_meta.
- **Checkpoint** (`ingest.go`): identity, parser version, zst bytes consumed
  and the SHA-256 of the 64 KB before them, parser `State`, generation, valid
  lengths of both logs, and the source's sorted session ids.
- **Part** (`build.go`). Fixed-width little-endian arrays, all mmapped:
  - `docs`: 40 B per event, in session window order.
  - `sessions`, `turns`, and `uids` (fnv64 to doc).
  - `terms` and `postings`.
  - `hot`: an uncompressed hit record per doc plus a text_content hash. A
    search or file_attention hit reads one record, never a log frame.
  - `fa.exact`, `fa.tio`, `fa.blob`, and the `fa.tri` trigram index.
  - `ls.json` rollups and `meta.json`.

Store for the arena raw (22 MB, 70 sources, 17.7k events): logs 29.7 MB and
62 parts at 30.7 MB, for 58 MB total. The DuckDB path takes 113 MB.

## Which structure serves each tool

- **search_sessions.**
  - df per query term is summed over every part's dictionary. N and total_len
    are summed from the part metas. This is the cache's exact idf, computed
    over unfiltered df.
  - Each part scores postings through `docs` flags and scope; the global top
    n+1 cut keeps ties; survivors read `hot`, `turns` and `sessions`.
- **file_attention.**
  - Exact pass: `fa.exact` in each part.
  - Suffix pass: the trigrams of the path's longest literal run, intersected
    in `fa.tri`, give a superset of the entries containing it. The same
    `bytes.Contains` and regex logic then runs on those entries only.
  - Regexps compile once per query; lists merge across parts, then sort and limit.
- **open.** The session → part map or a `uids` probe picks the part; the
  scan decodes that session's light frames in order.
- **list_sessions / get_ingest_status.** Parts' `sessions` + `ls.json`; `CURRENT` rows.

## Ingest, crash and recovery

1. **Parse.** Sources are parsed on GOMAXPROCS workers, one source per worker,
   each with only its own two logs and checkpoint.
   - Resume requires same identity and parser version, `zst_size` within the
     raw, an unchanged window hash, and zstd magic next. Then only the new
     frames are decoded, starting from the saved `State`.
   - Otherwise the generation is bumped and the source re-parses from byte 0.
   - Logs are truncated to the checkpoint's lengths, then appended and
     fsynced, then the checkpoint is renamed into place. A kill at any instant
     leaves either the old checkpoint (the junk tail is cut on the next run),
     or the new checkpoint over synced logs.
2. **Plan.** Union-find joins sources that share a session id.
   - Each component goes to shard `fnv32(least session id) % 256`.
   - A part is that shard's sources. Its name is the fingerprint of their
     checkpoints (key, gen, lengths, rows).
3. **Build only what is missing.** A part dir that exists is complete:
   it is built in `<id>.tmp`, fsynced, then renamed. Unchanged parts are
   reused; a killed build leaves a `.tmp` that GC removes.
   - Within a part, winners are: the highest version per uid within a source,
     then version desc and lake path across sources. Then come session order,
     turn_seq, ordinals and rollups.
4. **Commit.** `CURRENT` is rewritten (tmp+fsync+rename) only if its bytes
   changed, which makes ingest idempotent. After that, GC removes the part
   dirs and log generations the new state does not name. A batch still
   reading keeps its open files.

Rows with no record_ts get one fixed stamp (2100-01-01), never the wall clock.
A crash or split ingest therefore orders exactly like a clean one
(CONTRACT-NOTES 4). The plan rests on one assumption: a uid belongs to one
session. Every version that competes for a uid is then in the same part. If a
uid ever spans sessions, cross-part winners would not be resolved; nothing
checks for this.

## Freshness

`batch` serves the `CURRENT` it opened, fresh as of the last `ingest`. An append
costs a parse of its new zst frames plus a rebuild of its one part.

## Rejected alternatives

- **One global index, rebuilt in full.** This was the first version of this
  lane. Each ingest re-read every light log: 0.40 s and 123 MB when 1 of 70
  sources grew, which is O(corpus).
- **LSM tiers of per-source segments with tombstones.** An append changes old
  rows' turn counts, session bounds and winners. Tombstones would have to
  undo those, and correct BM25's df too. Replacing a whole session-closed part
  needs neither.
- **Re-reading payload from raw**: one zstd frame per append, `open` O(source).
- **Small light frames**, used to speed up hits. Search fell to 0.36 ms, but
  `open` rose to 8.6 to 11.7 ms and the light log grew 1.4 to 1.8x. `hot`
  replaced this.
- **SQLite or bbolt**: cgo or a dependency. **Clock ingested_at**: crash order.

## At 500x (8.8M events, 20 GB raw)

- **Merge policy.** There are no merges: a part is replaced whole. 256 fixed
  shards make about 34k events per part.
  - An ingest touching m sources rebuilds at most m parts.
  - Each rebuild reads that part's light logs (about 16 MB) and writes its
    index (about 60 MB). These sizes are scaled from 8.2 MB of light and
    30.7 MB of parts here (inferred).
  - So write amplification is fixed per touched part, not per appended byte.
    A 1 KB append costs about 60 MB of writes. That is the price of skipping
    tombstones.
  - Changing `partShards` re-plans everything once. When parts outgrow the
    budget, double it. Resharding is not automated.
- **Peak ingest RSS.** The bound is 4 parse workers plus one part build.
  - A worker holds parser state and about 2 frames, a few MB.
  - A build holds its part's light rows. The full corpus here peaked at
    190 MB, so call it at most 11 KB per event, or at most about 370 MB for a
    34k-event part (inferred).
  - A single session component larger than a shard is the unbounded case. It
    cannot be split without breaking turn_seq.
  - Grow-1 here peaks at 30 to 39 MB (measured).
- **Parser-version re-parse.** A new `normalize.Version` fails every resume
  test.
  - Each source re-parses from byte 0 into generation+1 logs, and every part
    fingerprint changes, so this is one full rebuild. Here, 22 MB parses in
    about 2.3 s on 4 workers, so 20 GB takes about 35 minutes (inferred).
  - A kill resumes per source. The old `CURRENT` and logs serve until the one
    commit; disk briefly doubles.
- **Idle batch RSS** is:
  - about 8 MB of runtime;
  - plus the session → part map, at about 120 B per session;
  - plus one frame cache shared across all parts, at most 48 light and 16
    heavy frames, a few tens of MB;
  - plus `ls.json`, at about 300 B per session, once list_sessions runs;
  - plus the mmapped pages queries touched, which the kernel can reclaim.
  - It grows with sessions and parts, not events. Each part costs two fds per source and 15 mappings, so 256 parts means
    about 3.8k mappings.
- **Latency.**
  - search fans out over 256 dictionaries per term, and exhaustive BM25 on
    common terms needs block-max WAND.
  - file_attention's trigram intersection stays small. Its cost moves to the
    per-candidate JSON and regex work.
- Directory fsync after renames is missing: needed for power loss, not kill -9.

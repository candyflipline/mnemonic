# arena/a: a DuckDB-free store

A single static binary, `a ingest|batch -root R -store S`. It is pure Go
(CGO_ENABLED=0), and its only non-std dependency is klauspost/compress for zstd.
The parsers are the repo's own (`internal/normalize`), and the tool code is
`internal/query` copied in with its SQL replaced by reads of the index
(`q_*.go`).

## On-disk formats

```
S/src/<author__machine__harness__srcid>.ckpt          JSON, written by tmp+fsync+rename
S/src/<key>.<gen>.light                               append-only frame log
S/src/<key>.<gen>.heavy                               append-only frame log
S/CURRENT                                             "index.N\n", written by tmp+fsync+rename
S/index.N/{docs,sessions,sessions.str,turns,uids,terms,terms.off,postings,
           fa.exact,fa.tio,fa.blob,ls.json,sources.json,meta.json}
```

- **Frame log.** A frame is: u32 compressed length, u32 record count, then
  zstd (level 3) of uvarint-length-prefixed records. Light frames hold about
  128 KB of plain records and heavy frames about 256 KB. A row is addressed by
  (source, frame offset, record index).
- **Light record** (`row.go`). Every column a tool reads except payload_json
  and token_usage_json, plus record_ts in µs, event_ts in ms, the
  moraine_tool_io fields, and where its heavy record is.
- **Heavy record.** payload_json and token_usage_json. Only `open` reads it,
  and only for rows it prints. At build, it is also read for session_meta rows.
- **Checkpoint** (`ingest.go`). Source identity, parser version, raw zst
  bytes consumed and the SHA-256 of the 64 KB before them, the parser `State`
  (a resumed parse continues exactly where it stopped), the generation, and
  the valid lengths of both logs.
- **Index** (`build.go`). Fixed-width little-endian arrays, mmapped read-only:
  - `docs`: 40 B per event in session order. It holds the frame, record,
    source, search flags, harness and source-name ids, session, turn_seq,
    ordinal, doc_len and event_time ms.
  - `sessions`: 32 B each, sorted by id, holding the doc range,
    first/last ms and completed.
  - `turns`: 24 B each, holding the count, completed and terminal doc.
  - `uids`: (fnv64, doc) pairs, sorted.
  - `terms` plus `terms.off`: a sorted dictionary.
  - `postings`: a uvarint count, then (doc gap, tf) pairs.
  - `fa.exact`: (repo_rel_path, doc) for tool requests that carry a project.
  - `fa.tio` plus `fa.blob`: the moraine_tool_io input_json of every tool
    request, uncompressed, so it can be scanned.
  - `ls.json`: list_sessions rollups for each session.
  - `sources.json`: the manifest rows behind get_ingest_status.

Store for the 22 MB arena raw (70 sources, 17.7k events): light 8.2 MB, heavy
21.7 MB, index 7.1 MB. Total 37 MB, against 113 MB for the DuckDB cache path.

## Which structure serves each tool

- **search_sessions.** Look up each query term in the dictionary (binary
  search), then walk its postings. Docs are filtered by the `docs` flags word
  (searchable bit plus event-type bits) and by session, turn, harness and
  source. BM25 is scored exhaustively with the cache's exact formula: idf over
  unfiltered df, and avgdl taken from meta. The top n+1 cut keeps ties.
  Survivors are hydrated from light frames only. Sessions and turns come from
  `sessions` and `turns`.
- **file_attention.** The exact pass is a scan of `fa.exact`. The suffix pass
  scans `fa.blob`, prefiltering on the literal runs of the path, then runs the
  query package's key, regex, shell and root logic on the candidates. Rows are
  hydrated from light frames.
- **open.** A session id resolves through `sessions`, an event id through
  `uids`. The session's doc range is already in window order with turn_seq
  and ordinals, so the scan decodes light frames sequentially. Only printed
  rows and session_meta rows read their heavy record.
- **list_sessions.** Filter `sessions` plus `ls.json` in memory by window,
  mode, harness and source. Order by last µs then id for the limit, then by
  ms for the page, which is the same two-step order as the SQL.
- **get_ingest_status.** `sources.json`, grouped by author and machine, fed
  to the copied derive code.

## Ingest, crash and recovery

1. List manifests. Sources are parsed on GOMAXPROCS workers, one source per
   worker. Each source's state is private: its two logs and its checkpoint.
2. **Resume test.** The checkpoint must have the same identity and parser
   version. Its `zst_size` must be no larger than the raw. The 64 KB window
   hash must be unchanged. The next 4 bytes must be zstd magic, or the end of
   file. If all of these hold, the parser decodes only the new frames, starting
   from the saved `State`. Otherwise the generation is bumped and the source is
   re-parsed from byte 0 into new logs.
3. Open both logs and truncate them to the checkpoint's valid lengths. This
   removes any tail a killed run wrote past its last checkpoint. Append the
   frames, fsync both logs, then write the checkpoint (tmp, fsync, rename).
   The checkpoint is the commit point. A kill at any instant leaves either the
   old checkpoint with junk past its lengths (truncated on the next run) or the
   new checkpoint with fully synced logs.
4. **Fingerprint.** Hash every checkpoint (key, gen, lengths, rows) plus the
   manifest rows. If it equals the current index's fingerprint, ingest is a
   no-op, which makes it idempotent.
5. **Build.**
   - Read all light logs and resolve winners: within a source, the highest
     event_version wins and the later row breaks a tie; across sources,
     version desc, then lake path.
   - Sort each session in window order. Assign turn_seq and ordinals; derive
     turns, sessions, postings, file_attention lists and rollups.
   - Write `index.tmp`, fsync every file, rename it to `index.N+1`, then
     write `CURRENT` by tmp+rename. A kill before the CURRENT rename leaves
     the old index serving. An orphan dir is overwritten next time.
6. **GC.** Remove the previous index dir, and every log generation no
   checkpoint names. A batch that is still reading keeps its open files, as
   POSIX allows.

Rows without a parseable record_ts get one fixed stamp (2100-01-01) instead
of a wall-clock ingested_at. It is later than every record_ts and identical
across runs, so a crash or split ingest orders exactly like a clean one
(CONTRACT-NOTES 4).

## Freshness

`batch` serves the index named by `CURRENT` when it opens. There is no
background work. Data is fresh as of the last completed `ingest`, and an
append costs a parse of only the new zst frames plus a full index rebuild
(the checker's final crash-run ingest took 1.0 s in total).

## Rejected alternatives

- **One global append log, with an LSM or segment merge for the index.**
  Incremental index updates need tombstones for re-parsed generations and
  winner changes. A full rebuild from light logs is simpler and fast at this
  size. It is the first thing to change at 100x (below).
- **Re-reading payload_json from raw** instead of a heavy log. The raw is one
  zstd frame per append, so random access means decompressing and re-parsing
  the whole source with parser state. That would save about 22 MB here but
  makes `open` cost O(source).
- **BM25 with WAND or block-max pruning.** The exhaustive scorer matches the
  SQL tie semantics (the n+1 cut keeps ties) trivially, and p99 is under 7 ms.
- **SQLite or bbolt**: cgo or a dependency, and a second format to keep
  consistent with the logs. **Wall-clock ingested_at**: breaks crash order.

## What changes at 100x (about 8.8M events, 20 GB raw)

- **Build memory.** Loading every light row (190 MB peak here) would be
  about 19 GB.
  - Partition the index by session hash, about 64 shards. Each shard is
    rebuilt only when one of its sessions changed, and the checkpoint already
    knows which sources moved.
  - Winner resolution needs a uid→(version, path) map across shards. That is
    about 8.8M × 16 B, which can be sorted on disk.
- **Postings** (about 200 MB) mmap fine. BM25 N, total_len and df become
  per-shard sums in a header. Common terms need block-max WAND to stay fast.
- **fa.blob** (about 270 MB) makes the suffix scan the slow path: index
  basename trigrams of input_json paths and scan only the candidates.
- **ls.json** becomes fixed-width. The heavy log (about 2 GB) is fine.
- **Ingest.** It stays proportional to new bytes for parse. With sharding,
  the rebuild becomes proportional to the sessions touched. Directory fsync
  after renames is needed for power loss; it is not needed for kill -9.

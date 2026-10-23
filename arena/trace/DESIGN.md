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
  - `terms`, `terms.idx`, `terms.filter` and `postings`. Terms are sorted and
    front-coded in blocks of 16, each entry carrying its postings offset and
    length; `terms.idx` holds each block's start, and `terms.filter` is a
    2 KiB bloom filter (3 probes), so a part without a query term costs three
    bit tests and no dictionary page.
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
   - A component with a hot source is a **hot part** of its own.
   - A settled component goes to the **cold part** of shard
     `fnv32(least session id) % 256`, with the other settled components there.
   - A part's name is the fingerprint of what its sources parsed (key, parser,
     layout, zst_size, window hash, rows), never of how a disk laid out the
     logs, so a settled source moving cold changes no name but its shard's and
     two disks that parsed the same raw name the same part.
3. **Build only what is missing.** A part dir that exists is complete:
   it is built in `<id>.tmp`, fsynced, then renamed. Unchanged parts are
   reused; a killed build leaves a `.tmp` that GC removes.
   - Within a part, winners are: the highest version per uid within a source,
     then version desc and lake path across sources. Then come session order,
     turn_seq, ordinals and rollups.
4. **Commit.** Every rename of a checkpoint, part or `CURRENT` fsyncs the
   parent directory before (so the entries of new logs or part files are
   durable first) and after. `CURRENT` is rewritten (tmp+fsync+rename) only if its bytes
   changed, which makes ingest idempotent. After that, GC removes the part
   dirs and log generations the new state does not name. A batch still
   reading keeps its open files.

Rows with no record_ts get one fixed stamp (2100-01-01), never the wall clock.
A crash or split ingest therefore orders exactly like a clean one
(CONTRACT-NOTES 4). The plan rests on one assumption: a uid belongs to one
session. Every version that competes for a uid is then in the same part. If a
uid ever spans sessions, cross-part winners would not be resolved; nothing
checks for this.

## Restore

A cold part's `meta.json` records each member's `parsed` record: the
checkpoint minus the local log fields (a cold part carries Gen and the log
lengths as zero, so its bytes are the same on every disk). `RestorePart`
turns a packed part back into local state:

1. The blob is fetched to `parts/.fetch-<id>`, checked against its SHA-256 and
   unpacked into `parts/<id>.tmp`, then renamed (`commitPart`). A dir that
   exists is skipped.
2. Each member with no checkpoint gets `src/<key>.0.light|heavy` re-framed from
   the part's own frames (its docs whose src is that member, in doc order, heavy
   records placed again), fsynced, and then a checkpoint by rename: the parsed
   record, gen 0, the new lengths, `cold_zst = zst_size`, `restored: true`.
   A member with a checkpoint is left alone. A part without parse records
   returns `ErrNoParseRecord`.
3. A crash leaves a `.fetch-*` file or `.tmp` dir (removed on the next run), or
   logs with no checkpoint (rewritten on the next run). Rerunning converges.

Ingest then resumes every restored source by the usual test, so an unchanged
raw parses nothing (`IngestStats{N,0,0,0}`) and `planParts` names the same
parts. A part rebuilt from restored logs packs to the same bytes as the
original: winner selection is a max, so the winners alone give the same
winners.

That holds only while no source is reparsed. A re-framed log holds only its
part's winners, so when a source is `Reparsed` (identity, window or parser
mismatch) every `restored` member of its component, taken over the sessions it
had before and after the parse, parses from raw into gen+1 too and counts
`Reparsed`. A source parsed from raw clears `restored`.

Known limitation: while a source is hot (appended within `-quiet`), its
component is not in a committed manifest at all, so a committed-only answer
can miss currently active sessions entirely, not just their newest events. A
durable plan that retains the committed prefix until the component settles is
the follow-up.

## Hot and cold

A source is **hot** while its checkpoint has grown since it last settled:
`zst_size != cold_zst` in the checkpoint. It **settles** at the first ingest
where its manifest `updated_ms` is at least `-quiet` (default 15 min) behind
the newest `updated_ms` in the root, or at any ingest run with `-settle`.
Settling writes `cold_zst = zst_size` into the checkpoint (tmp, fsync,
rename).

- The clock is the server's receive stamp in the raw manifests, never the
  ingest's wall clock. The layout is then a function of raw, the checkpoints
  and the flags, and a re-run with nothing new changes nothing.
- Settling is sticky: a plain run after `-settle` keeps everything cold.
- A settled source that is appended to again is hot again. Its component
  leaves its cold part, which is rebuilt once without it, and becomes a hot
  part. When it goes quiet, the cold part is rebuilt once more with it.
- A fresh or re-parsed source starts hot (`cold_zst` 0) and settles in the
  same run when it is already quiet, so a full rebuild lands mostly cold.
- Results do not depend on the layout. Winners stay inside one component,
  BM25 sums per-part stats, and every merge sorts on score, time and uid.
  The checker's root has one `updated_ms` for every source, so every source
  stays hot there (70 single-component parts). A `-settle` store answers
  the 63 public calls identically (scratch run).

So an append rebuilds only its component's hot part, and a cold part is
rewritten only when a component settles into it or leaves it.

## Freshness

`batch` stats `CURRENT` before every call. When its inode changed, it opens
the new manifest, then unmaps and closes the old parts. Calls run one at a
time, so no call is in flight across the swap. If the open fails, for
example because a later ingest removed a part in between, the old snapshot
keeps serving and the next call retries.

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
  cold shards make about 34k events per part, plus one hot part per active
  component.
  - An append rebuilds only its component's hot part: O(that session), not
    O(shard).
  - A cold shard is rewritten once when a component settles into it and once
    when a settled one wakes up. Each rewrite reads the shard's light logs
    (about 16 MB) and writes its index (about 60 MB), scaled from 8.2 MB of
    light and 30.7 MB of parts here (inferred).
  - Measured here with every source in one 17.7k-event cold shard
    (`partShards = 1`, about half a 500x shard) and 20 appends of 83 B to
    25 KB to one source: before, each append that adds an event writes
    23.2 MB at 136 to 158 MB RSS (20 appends, 301.7 MB). After, the first
    append writes 23.3 MB at 132 MB RSS (its component leaves the shard),
    then 237 to 268 KB at 22 to 25 MB RSS (20 appends, 26.5 MB). Appends that
    end mid-line write about 7 KB (log and checkpoint only). At 256 shards
    this sample puts the source alone in its shard, so before and after both
    write 3.3 MB over the 20 appends.
  - Not bounded: a long active session rebuilds its whole hot part on every
    append; a component that joins a hot source to a large settled one is hot
    as a whole; many sessions settling in one ingest rewrite that many cold
    shards; hot parts add to the part count search fans out over.
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
  - search fans out over 256 cold and the hot dictionaries per term. The
    term filter skips a part that lacks the term with three bit tests; on
    this sample's 70 parts it bought no measured search speed (p50/p99
    166/778 µs before, 174/919 µs after, on a loaded machine) and 2.1 MB of
    store. Exhaustive BM25 on common terms needs block-max WAND.
  - file_attention's trigram intersection stays small. Its cost moves to the
    per-candidate JSON and regex work.

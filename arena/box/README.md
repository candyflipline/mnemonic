# arena/box — real-corpus baseline scripts (mnemonic-e3)

Scripts that build an arena candidate on the box and time it over the real
corpus, so a later lane reruns them unchanged against the engine binary. Every
script takes the binary under test as its first argument and everything else
through `BOX`/`RUNS`-style env with the same defaults. They are POSIX sh, run
as root on the box, and never print corpus content — counts, sizes and timings
only. Logs land in `$BOX/out/`; the ones scp'd back to the Mac carry counts and
timings only.

## Box layout (BOX defaults to /var/lib/mnemonic-e3)

```
$BOX/raw                     the frozen corpus (read-only; scripts never write here)
$BOX/bin-<tree>/             binaries built by build.sh from that tree + sha256.txt
                             + wrapper-<name> (GOMAXPROCS=16 launcher for check's -bin)
$BOX/cold-N                  stores from ingest-timed.sh; cold-1 is the gen root
                             (GEN_ROOT) and cold-2 the masks diff store (MASK_ROOT);
                             append-latency and call-latency default to cold-1 too
$BOX/arena/                  the fixture check scores: golden/, hidden/, check/,
                             runs/, and raw/ — a DATAROOT whose raw/raw symlinks
                             to the frozen corpus (check's clean phase runs
                             `ingest -root <arena>/raw`, so <arena>/raw/raw must exist)
$BOX/append/, $BOX/out/      scratch and logs
```

## Order

1. `build.sh TARBALL TREE` — build oracle/gen (cgo) and check/seed from a git
   archive tar; sha256 into `$BOX/bin-<tree>/sha256.txt`. Also installs the
   `/usr/bin/time` shim (below) and the GOMAXPROCS wrappers.
2. `ingest-timed.sh BIN` — cold full-corpus ingests (fresh store, page cache
   dropped, GOMAXPROCS=16, no GOMEMLIMIT) with vmstat/pidstat alongside;
   RUNS runs starting at FIRST (defaults 3 and 1). The oracle baseline on the
   box was 2 runs (a single-core parse, ~1 h each); the engine lane adds runs
   alternating.
3. `gen-masks.sh BIN` — gen seed 1 (golden) and seed 2 (hidden) from the
   gen root; masks by answering the same calls from the second store and diffing with
   `check masks`; then the oracle self-check (`check -bin wrapper -skip
   incremental,crash,appendcrash`), whose clean phase is one more full ingest.
4. `append-latency.sh BIN` — 5 rounds of: fresh hardlink overlay of the raw
   (target's two files unlinked and copied first — the frozen raw is never
   written), a 10-line append as one new zstd frame with the manifest's
   zst_size/size/raw_end/updated_ms updated, a fresh store, timed `ingest` +
   timed one-call `batch search_sessions` that must still return the
   pre-append top-hit session and must differ from the pre-append reply.
5. `call-latency.sh BIN` — 5 fresh `batch` processes over golden+hidden;
   per-tool p50/p95 of the `us` field over both sets combined.

## Deviations from the harness's Mac assumptions, made on the box

- `arena/check` runs `/usr/bin/time -l` (arena/check/main.go:180). `-l` is a
  BSD flag; GNU time on Linux exits `invalid option`. build.sh keeps the real
  binary as `/usr/bin/time.gnu` and installs a shim translating only `-l` into
  a GNU `-f` format printing the max-RSS line check's parser reads
  (`%M maximum resident set size`). Measurement semantics unchanged.
- `check masks` needs two full stores of the same corpus. ingest-timed.sh
  already produces two (cold-1, cold-2); gen-masks.sh diffs those instead of
  building a dedicated second store.
- check's clean phase requires `<arena>/raw/raw` (a dataroot under the arena),
  not `<arena>/raw` being the corpus dir itself.

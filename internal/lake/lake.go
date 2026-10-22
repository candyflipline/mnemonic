package lake

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/klauspost/compress/zstd"
)

// tsFormat is upstream's DateTime64(3) rendering: millisecond UTC timestamps
// as plain strings, and Date values as their first ten characters.
const tsFormat = "2006-01-02 15:04:05.000"

const epochIngestedAt = "1970-01-01 00:00:00.000"

type Stats struct {
	Sources, Rewrote, Unchanged, RawOnly, Failed int
	Events                                       int64
	// Stale counts the idle sources the parser sweep has not visited in this
	// lap; Next is where to resume it, the zero Sweep when the lap is over.
	Stale int
	Next  Sweep
}

// Sweep is where a parser sweep lap stands: the first idle source (by manifest
// path) not visited yet.
type Sweep struct {
	From string
}

type manifest struct {
	wire.Source
	ZstSize int64 `json:"zst_size"`
}

// Run parses every raw source whose manifest UpdatedMs is newer than its lake
// files (or has none) with normalize.For(harness), from the checkpoint when it
// still holds and from the start otherwise, stamps author, machine_id and
// ingested_at, dedupes event_uid by highest event_version, and writes
// events/... via DuckDB COPY (zstd) to a temp file then rename. A harness
// without a parser counts as RawOnly. A second run rewrites 0.
//
// While events/.parser-version differs from normalize.Version, Run also
// visits, after the pushed sources, the unchanged ones in manifest-path order
// from sweep.From: a source whose lake file records another parser version is
// re-parsed, one already current is passed. It stops, with Stats.Next set to
// the first source not visited, when yield returns true or sweepBudget has
// elapsed, never before the first visit, so pushes arriving faster than a
// cycle still advance the lap. When it reaches the end of the
// list it writes the marker and returns the zero Next. The marker means every
// source was tried at this version, not that every one parsed: a source that
// failed stays on its older output and a push or the next version bump
// parses it again, so one poison source cannot keep the lake sweeping. A
// crash before the marker leaves it old, and a later lap redoes only what the
// checkpoints still show stale.
func Run(dataDir string, yield func() bool, sweep Sweep) (Stats, error) {
	rawDir := filepath.Join(dataDir, "raw")
	if _, err := os.Stat(rawDir); err != nil {
		if os.IsNotExist(err) {
			return Stats{}, nil
		}
		return Stats{}, err
	}
	paths, err := filepath.Glob(filepath.Join(rawDir, "author=*", "machine=*", "harness=*", "*.json"))
	if err != nil {
		return Stats{}, err
	}
	sort.Strings(paths)
	db, err := open()
	if err != nil {
		return Stats{}, err
	}
	defer db.Close()
	ingestedAt := time.Now().UTC().Format(tsFormat)

	var st Stats
	parse := func(mpath string, key layout.Key, src manifest, parser normalize.Parser) {
		written, n, err := parseAndWrite(db, dataDir, key, src, parser, ingestedAt)
		if err == nil {
			err = stamp(written, src.UpdatedMs)
		}
		if err != nil {
			st.Failed++
			fmt.Fprintf(os.Stderr, "lake: %s: %v\n", mpath, err)
			return
		}
		st.Rewrote++
		st.Events += n
	}
	type idle struct {
		mpath string
		key   layout.Key
		src   manifest
	}
	sweeping := readParserMarker(dataDir) != normalize.Version
	var idles []idle
	for _, mpath := range paths {
		st.Sources++
		src, key, err := readManifest(rawDir, mpath)
		if err != nil {
			st.Failed++
			fmt.Fprintf(os.Stderr, "lake: %s: %v\n", mpath, err)
			continue
		}
		if lakeMtime(key, dataDir) >= src.UpdatedMs {
			if sweeping {
				idles = append(idles, idle{mpath, key, src})
			} else {
				st.Unchanged++
			}
			continue
		}
		parser, ok := normalize.For(src.Harness)
		if !ok {
			st.RawOnly++
			continue
		}
		parse(mpath, key, src, parser)
	}

	start := sort.Search(len(idles), func(i int) bool { return idles[i].mpath >= sweep.From })
	st.Unchanged += start
	sweepEnd := time.Now().Add(sweepBudget)
	n := start
	for ; n < len(idles); n++ {
		if n > start && (yield() || time.Now().After(sweepEnd)) {
			break
		}
		i := idles[n]
		parser, ok := normalize.For(i.src.Harness)
		if cp, has := readCheckpoint(db, i.key.Events(dataDir)); ok && (!has || cp.Parser != normalize.Version) {
			parse(i.mpath, i.key, i.src, parser)
		} else {
			st.Unchanged++
		}
	}
	if n < len(idles) {
		st.Unchanged += len(idles) - n
		st.Stale = len(idles) - n
		st.Next = Sweep{From: idles[n].mpath}
		return st, nil
	}
	if sweeping {
		if err := writeParserMarker(dataDir); err != nil {
			return st, err
		}
	}
	return st, nil
}

// sweepBudget bounds the wall time one Run spends on a sweep, checked before
// each visit after the first. The converge after Run must apply every file the
// sweep wrote before the next push's cycle can start, so this caps that
// converge. It does not bound the lap: Run's caller starts the next cycle at
// once while Stale > 0, from Next, and Run stops after one visit when yield says
// a push is pending.
const sweepBudget = 5 * time.Second

// readParserMarker is the normalize.Version the whole lake was last fully
// swept at, 0 when there is no marker.
func readParserMarker(dataDir string) int {
	b, err := os.ReadFile(layout.ParserMarker(dataDir))
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return v
}

func writeParserMarker(dataDir string) error {
	path := layout.ParserMarker(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(normalize.Version)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ImportClickHouse reads per-month parquet exports of the retired ClickHouse
// events (all 59 columns) from exportDir into
// events/author=<author>/machine=archive-ch/,
// one ch-<export base>.parquet per harness in the export. Rows whose
// source_file already lives in a non-archive parquet of that author are
// skipped; everything else is rewritten on every call, so reruns converge.
func ImportClickHouse(dataDir, exportDir, author string) (Stats, error) {
	exports, err := filepath.Glob(filepath.Join(exportDir, "*.parquet"))
	if err != nil {
		return Stats{}, err
	}
	db, err := open()
	if err != nil {
		return Stats{}, err
	}
	defer db.Close()
	live, err := liveSourceFiles(db, dataDir, author)
	if err != nil {
		return Stats{}, err
	}

	var st Stats
	for _, exp := range exports {
		st.Sources++
		harnesses, err := harnessesIn(db, exp)
		if err != nil {
			st.Failed++
			fmt.Fprintf(os.Stderr, "lake: %s: %v\n", exp, err)
			continue
		}
		for _, h := range harnesses {
			out := filepath.Join(dataDir, "events",
				"author="+author, "machine="+layout.ArchiveMachine, "harness="+h,
				"ch-"+strings.TrimSuffix(filepath.Base(exp), ".parquet")+".parquet")
			n, err := importHarness(db, exp, h, out, author, live)
			if err != nil {
				st.Failed++
				fmt.Fprintf(os.Stderr, "lake: %s: %v\n", exp, err)
				continue
			}
			st.Rewrote++
			st.Events += n
		}
	}
	return st, nil
}

func readManifest(rawDir, mpath string) (manifest, layout.Key, error) {
	var src manifest
	b, err := os.ReadFile(mpath)
	if err != nil {
		return src, layout.Key{}, err
	}
	if err := json.Unmarshal(b, &src); err != nil {
		return src, layout.Key{}, err
	}
	rel, err := filepath.Rel(rawDir, mpath)
	if err != nil {
		return src, layout.Key{}, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 4 ||
		!strings.HasPrefix(parts[0], "author=") ||
		!strings.HasPrefix(parts[1], "machine=") ||
		!strings.HasPrefix(parts[2], "harness=") {
		return src, layout.Key{}, fmt.Errorf("unexpected manifest path %s", rel)
	}
	key := layout.Key{
		Author:  strings.TrimPrefix(parts[0], "author="),
		Machine: strings.TrimPrefix(parts[1], "machine="),
		Harness: strings.TrimPrefix(parts[2], "harness="),
		SrcID:   strings.TrimSuffix(parts[3], ".json"),
	}
	return src, key, nil
}

// checkpoint is where a source's parse stopped: the raw's compressed length,
// the SHA-256 of the window bytes ending there, the parser State there, and
// the highest event_version written. It lives in the key-value metadata of the
// tail when there is one and of the base otherwise, so the rows and the
// position they were parsed to are replaced by one rename. A tail's checkpoint
// also records the base ZstSize it extends. Store changes bytes
// below ZstSize only by a replace, which renames a new raw into place; an
// append truncates to ZstSize and writes a new frame past it. So a raw whose
// window still hashes the same and holds a zstd frame at ZstSize is this one
// grown by appends. A file parsed under another normalize.Version or source
// identity, or failing either check, is parsed whole again.
type checkpoint struct {
	Parser       int             `json:"parser"`
	Harness      string          `json:"harness"`
	SourceName   string          `json:"source_name"`
	SourceFile   string          `json:"source_file"`
	ZstSize      int64           `json:"zst_size"`
	WindowSHA256 string          `json:"window_sha256"`
	State        normalize.State `json:"state"`
	MaxVersion   uint64          `json:"max_version"`
	BaseZstSize  int64           `json:"base_zst_size"`
}

func (cp checkpoint) sameSource(o checkpoint) bool {
	return cp.Parser == o.Parser && cp.Harness == o.Harness &&
		cp.SourceName == o.SourceName && cp.SourceFile == o.SourceFile
}

const checkpointKey = "mnemonic_lake"

const window = 64 << 10

var zstdFrameMagic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}

const compactAt = 8 << 20

const rowGroupBytes = 64 << 20

// parseAndWrite parses the raw bytes the checkpoint has not seen, from its
// State, and returns the lake file it wrote and that file's row count. A
// resumed parse rewrites only the tail, as its rows minus any event_uid the
// new rows supersede plus the new rows, until the tail on disk reaches
// min(compactAt, base size); then the base absorbs the tail and the new rows.
// New rows take an event_version above every row already written, so a tail
// row beats the base row it re-emits. Without a usable checkpoint it parses
// the whole raw into the base. A tail that does not extend the base is
// deleted first, and the tail is always deleted before a base rename, so a
// crash at any point leaves files the next run converges from.
func parseAndWrite(db *sql.DB, dataDir string, key layout.Key, src manifest, parser normalize.Parser, ingestedAt string) (string, int64, error) {
	raw, err := os.Open(key.Raw(dataDir))
	if err != nil {
		return "", 0, err
	}
	defer raw.Close()
	basePath, tailPath := key.Events(dataDir), key.Tail(dataDir)
	next := checkpoint{Parser: normalize.Version, Harness: src.Harness, SourceName: src.SourceName, SourceFile: src.Path}
	baseCP, hasBase := readCheckpoint(db, basePath)
	tailCP, hasTail := readCheckpoint(db, tailPath)
	if !hasBase || !hasTail || !tailCP.sameSource(baseCP) || tailCP.BaseZstSize != baseCP.ZstSize {
		if err := os.Remove(tailPath); err != nil && !os.IsNotExist(err) {
			return "", 0, err
		}
		hasTail = false
	}
	cp := baseCP
	if hasTail {
		cp = tailCP
	}
	from, resumed := resume(raw, src, next, cp)

	zr, err := zstd.NewReader(io.NewSectionReader(raw, from.ZstSize, src.ZstSize-from.ZstSize))
	if err != nil {
		return "", 0, err
	}
	defer zr.Close()
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		return "", 0, err
	}
	ndjson, err := os.CreateTemp(filepath.Dir(basePath), ".lake-*.ndjson")
	if err != nil {
		return "", 0, err
	}
	tmpName := ndjson.Name()
	defer os.Remove(tmpName)
	w := bufio.NewWriterSize(ndjson, 1<<20)
	type seen struct{ n, version uint64 }
	winners := map[string]seen{}
	var losers []string
	var row uint64
	next.MaxVersion = from.MaxVersion
	state, err := parser.Parse(zr, normalize.SourceMeta{
		SourceName: src.SourceName,
		Harness:    src.Harness,
		SourceFile: src.Path,
	}, from.State, func(ev *schema.Event) error {
		if prev, ok := winners[ev.EventUID]; ok && ev.EventVersion < prev.version {
			losers = append(losers, strconv.FormatUint(row, 10))
		} else {
			if ok {
				losers = append(losers, strconv.FormatUint(prev.n, 10))
			}
			winners[ev.EventUID] = seen{row, ev.EventVersion}
		}
		if resumed {
			ev.EventVersion = max(ev.EventVersion, from.MaxVersion+1)
		}
		next.MaxVersion = max(next.MaxVersion, ev.EventVersion)
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, `{"_n":%d,`, row)
		w.Write(b[1:])
		row++
		return w.WriteByte('\n')
	})
	if err != nil {
		ndjson.Close()
		return "", 0, err
	}
	if err := w.Flush(); err != nil {
		ndjson.Close()
		return "", 0, err
	}
	if err := ndjson.Close(); err != nil {
		return "", 0, err
	}
	sum, err := windowSHA256(raw, src.ZstSize)
	if err != nil {
		return "", 0, err
	}
	next.ZstSize, next.WindowSHA256, next.State = src.ZstSize, sum, state

	fresh := fmt.Sprintf("SELECT %s FROM read_json(%s, format='newline_delimited', columns={'_n': 'UBIGINT', %s)",
		selectList(key.Author, key.Machine, ingestedAt), sqlQuote(tmpName), strings.TrimPrefix(columnTypes(), "{"))
	if len(losers) > 0 {
		fresh += " WHERE _n NOT IN (" + strings.Join(losers, ", ") + ")"
	}

	out, query, drop := basePath, fresh, tailPath
	switch {
	case !resumed:
	case !hasTail:
		out, query, drop = tailPath, fresh, ""
		next.BaseZstSize = from.ZstSize
	case !compactDue(basePath, tailPath):
		out, drop = tailPath, ""
		query = fmt.Sprintf("WITH fresh AS (%s) SELECT * FROM %s "+
			"WHERE event_uid NOT IN (SELECT event_uid FROM fresh) UNION ALL SELECT * FROM fresh",
			fresh, lakeRows(tailPath))
		next.BaseZstSize = tailCP.BaseZstSize
	default:
		query = fmt.Sprintf("WITH fresh AS (%s), tail AS (SELECT * FROM %s) SELECT * FROM %s "+
			"WHERE event_uid NOT IN (SELECT event_uid FROM fresh UNION ALL SELECT event_uid FROM tail) "+
			"UNION ALL SELECT * FROM tail WHERE event_uid NOT IN (SELECT event_uid FROM fresh) "+
			"UNION ALL SELECT * FROM fresh",
			fresh, lakeRows(tailPath), lakeRows(basePath))
	}
	kv, err := json.Marshal(next)
	if err != nil {
		return "", 0, err
	}
	n, err := copyParquet(db, query, out, string(kv), drop)
	return out, n, err
}

func lakeRows(path string) string {
	return "read_parquet(" + sqlQuote(path) + ", hive_partitioning=false)"
}

func compactDue(basePath, tailPath string) bool {
	b, berr := os.Stat(basePath)
	t, terr := os.Stat(tailPath)
	return berr == nil && terr == nil && t.Size() >= min(compactAt, b.Size())
}

// lakeMtime is the newest mtime of a source's lake files in ms, or 0 without
// a base.
func lakeMtime(key layout.Key, dataDir string) int64 {
	b, err := os.Stat(key.Events(dataDir))
	if err != nil {
		return 0
	}
	ms := b.ModTime().UnixMilli()
	if t, err := os.Stat(key.Tail(dataDir)); err == nil {
		ms = max(ms, t.ModTime().UnixMilli())
	}
	return ms
}

// resume returns cp and true when it belongs to this binary and source, its
// State is not Provisional, and raw still extends it: the window ending at its ZstSize hashes the same, and
// a zstd frame starts at ZstSize unless raw ends there. Otherwise it returns
// the zero checkpoint, a parse from the start.
func resume(raw *os.File, src manifest, want, cp checkpoint) (checkpoint, bool) {
	if !cp.sameSource(want) || cp.ZstSize > src.ZstSize || cp.State.Provisional() {
		return checkpoint{}, false
	}
	if sum, err := windowSHA256(raw, cp.ZstSize); err != nil || sum != cp.WindowSHA256 {
		return checkpoint{}, false
	}
	if cp.ZstSize < src.ZstSize {
		var magic [4]byte
		if _, err := raw.ReadAt(magic[:], cp.ZstSize); err != nil || magic != zstdFrameMagic {
			return checkpoint{}, false
		}
	}
	return cp, true
}

// windowSHA256 hashes raw's window bytes ending at end, fewer when end is
// nearer the start.
func windowSHA256(raw *os.File, end int64) (string, error) {
	start := max(0, end-window)
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(raw, start, end-start), end-start); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readCheckpoint(db *sql.DB, eventsPath string) (checkpoint, bool) {
	var cp checkpoint
	if _, err := os.Stat(eventsPath); err != nil {
		return cp, false
	}
	var b []byte
	err := db.QueryRow(fmt.Sprintf("SELECT value FROM parquet_kv_metadata(%s) WHERE decode(key) = %s",
		sqlQuote(eventsPath), sqlQuote(checkpointKey))).Scan(&b)
	if err != nil || json.Unmarshal(b, &cp) != nil {
		return checkpoint{}, false
	}
	return cp, true
}

// stamp sets written's mtime to the millisecond of parsedMs, the manifest
// UpdatedMs it was parsed from, and below it the write's own nanoseconds.
func stamp(written string, parsedMs int64) error {
	t := time.UnixMilli(parsedMs).Add(time.Duration(time.Now().Nanosecond() % 1e6))
	return os.Chtimes(written, t, t)
}

func liveSourceFiles(db *sql.DB, dataDir, author string) (map[string]bool, error) {
	glob := filepath.Join(dataDir, "events", "author="+author, "machine=*", "harness=*", "*.parquet")
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		return map[string]bool{}, err
	}
	_, err = db.Exec(fmt.Sprintf(
		"CREATE OR REPLACE TEMP TABLE live_files AS SELECT DISTINCT source_file FROM read_parquet(%s) WHERE machine_id <> %s",
		sqlQuote(glob), sqlQuote(layout.ArchiveMachine)))
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT source_file FROM live_files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		live[f] = true
	}
	return live, rows.Err()
}

func harnessesIn(db *sql.DB, export string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf("SELECT DISTINCT harness FROM read_parquet(%s)", sqlQuote(export)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func importHarness(db *sql.DB, export, harness, out, author string, live map[string]bool) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return 0, err
	}
	query := fmt.Sprintf("SELECT %s FROM read_parquet(%s) WHERE harness = %s AND source_file IS NOT NULL",
		selectList(author, layout.ArchiveMachine, epochIngestedAt), sqlQuote(export), sqlQuote(harness))
	if len(live) > 0 {
		query += " AND source_file NOT IN (SELECT source_file FROM live_files)"
	}
	return copyParquet(db, query, out, "", "")
}

func open() (*sql.DB, error) { return sql.Open("duckdb", "?threads=1") }

// copyParquet writes query to outPath through a temp file and a rename, with
// kv, when set, as the checkpoint metadata, removes drop, when set, between
// the two, and returns the rows written.
func copyParquet(db *sql.DB, query, outPath, kv, drop string) (int64, error) {
	tmp := outPath + ".tmp"
	opts := fmt.Sprintf("FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE_BYTES %d", rowGroupBytes)
	if kv != "" {
		opts += fmt.Sprintf(", KV_METADATA {%s: %s}", checkpointKey, sqlQuote(kv))
	}
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "SET preserve_insertion_order = false"); err != nil {
		return 0, err
	}
	res, err := c.ExecContext(ctx, fmt.Sprintf("COPY (%s) TO %s (%s)", query, sqlQuote(tmp), opts))
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if drop != "" {
		if err := os.Remove(drop); err != nil && !os.IsNotExist(err) {
			os.Remove(tmp)
			return 0, err
		}
	}
	return n, os.Rename(tmp, outPath)
}

// selectList is the lake's column order and types: every schema.EventColumns
// column cast to its DuckDB type, then the three server-stamped columns.
// Explicit casts absorb whatever the ClickHouse export's parquet dialect
// calls LowCardinality, DateTime64 and Map.
func selectList(author, machine, ingestedAt string) string {
	var b strings.Builder
	for _, c := range schema.EventColumns {
		fmt.Fprintf(&b, "CAST(%s AS %s) AS %s, ", quoteIdent(c.Name), c.DuckDB, quoteIdent(c.Name))
	}
	fmt.Fprintf(&b, "%s AS author, %s AS machine_id, TIMESTAMP %s AS ingested_at",
		sqlQuote(author), sqlQuote(machine), sqlQuote(ingestedAt))
	return b.String()
}

// columnTypes is read_json's columns spec: the DuckDB type of every column,
// except the maps, which stay JSON here and are cast by selectList (JSON
// objects are not always accepted as MAP literals by the reader).
func columnTypes() string {
	var b strings.Builder
	b.WriteString("{")
	for i, c := range schema.EventColumns {
		if i > 0 {
			b.WriteString(", ")
		}
		t := c.DuckDB
		if strings.HasPrefix(t, "MAP(") {
			t = "JSON"
		}
		fmt.Fprintf(&b, "%s: %s", sqlQuote(c.Name), sqlQuote(t))
	}
	return b.String() + "}"
}

func sqlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

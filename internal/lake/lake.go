package lake

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
}

type manifest struct {
	wire.Source
	ZstSize int64 `json:"zst_size"`
}

// Run parses every raw source whose manifest UpdatedMs is newer than its
// parquet (or has none) with normalize.For(harness), from the parquet's
// checkpoint when it still holds and from the start otherwise, stamps author,
// machine_id and ingested_at, dedupes event_uid by highest event_version, and
// writes events/... via DuckDB COPY (zstd) to a temp file then rename. A
// harness without a parser counts as RawOnly. A second run rewrites 0.
func Run(dataDir string) (Stats, error) {
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
	db, err := open()
	if err != nil {
		return Stats{}, err
	}
	defer db.Close()
	ingestedAt := time.Now().UTC().Format(tsFormat)

	var st Stats
	for _, mpath := range paths {
		st.Sources++
		src, key, err := readManifest(rawDir, mpath)
		if err != nil {
			st.Failed++
			fmt.Fprintf(os.Stderr, "lake: %s: %v\n", mpath, err)
			continue
		}
		eventsPath := key.Events(dataDir)
		if info, err := os.Stat(eventsPath); err == nil && info.ModTime().UnixMilli() >= src.UpdatedMs {
			st.Unchanged++
			continue
		}
		parser, ok := normalize.For(src.Harness)
		if !ok {
			st.RawOnly++
			continue
		}
		n, err := parseAndWrite(db, dataDir, key, src, parser, ingestedAt)
		if err != nil {
			st.Failed++
			fmt.Fprintf(os.Stderr, "lake: %s: %v\n", mpath, err)
			continue
		}
		if err := restampIfSuperseded(mpath, eventsPath, src); err != nil {
			st.Failed++
			fmt.Fprintf(os.Stderr, "lake: %s: %v\n", mpath, err)
			continue
		}
		st.Rewrote++
		st.Events += n
	}
	return st, nil
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

// checkpoint is where a lake file's parse stopped: the raw's compressed
// length and its SHA-256 at that point, and the parser State there. It lives
// in the parquet's key-value metadata, so the rows and the position they were
// parsed to are replaced by one rename. A file parsed by another binary or
// under another source identity, or whose raw prefix no longer hashes the
// same (a replace), is parsed whole again.
type checkpoint struct {
	Binary     string          `json:"binary"`
	Harness    string          `json:"harness"`
	SourceName string          `json:"source_name"`
	SourceFile string          `json:"source_file"`
	ZstSize    int64           `json:"zst_size"`
	ZstSHA256  string          `json:"zst_sha256"`
	State      normalize.State `json:"state"`
}

const checkpointKey = "mnemonic_lake"

var binaryID = sync.OnceValue(func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
})

// parseAndWrite parses the raw bytes the parquet's checkpoint has not seen,
// from the checkpoint's State, and rewrites the parquet as its earlier rows
// minus any event_uid the new rows supersede, plus the new rows. Without a
// usable checkpoint it parses the whole raw.
func parseAndWrite(db *sql.DB, dataDir string, key layout.Key, src manifest, parser normalize.Parser, ingestedAt string) (int64, error) {
	raw, err := os.Open(key.Raw(dataDir))
	if err != nil {
		return 0, err
	}
	defer raw.Close()
	eventsPath := key.Events(dataDir)
	next := checkpoint{Binary: binaryID(), Harness: src.Harness, SourceName: src.SourceName, SourceFile: src.Path}
	from, h, err := resume(db, eventsPath, raw, src, next)
	if err != nil {
		return 0, err
	}
	tail := io.TeeReader(io.LimitReader(raw, src.ZstSize-from.ZstSize), h)
	zr, err := zstd.NewReader(tail)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	events, state, err := parser.Parse(zr, normalize.SourceMeta{
		SourceName: src.SourceName,
		Harness:    src.Harness,
		SourceFile: src.Path,
	}, from.State)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(io.Discard, tail); err != nil {
		return 0, err
	}
	events = dedupe(events)
	next.ZstSize, next.ZstSHA256, next.State = src.ZstSize, hex.EncodeToString(h.Sum(nil)), state

	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		return 0, err
	}
	ndjson, err := os.CreateTemp(filepath.Dir(eventsPath), ".lake-*.ndjson")
	if err != nil {
		return 0, err
	}
	tmpName := ndjson.Name()
	defer os.Remove(tmpName)
	enc := json.NewEncoder(ndjson)
	for i := range events {
		if err := enc.Encode(&events[i]); err != nil {
			ndjson.Close()
			return 0, err
		}
	}
	if err := ndjson.Close(); err != nil {
		return 0, err
	}
	query := fmt.Sprintf("SELECT %s FROM read_json(%s, format='newline_delimited', columns=%s)",
		selectList(key.Author, key.Machine, ingestedAt), sqlQuote(tmpName), columnTypes())
	if from.ZstSize > 0 {
		query = fmt.Sprintf("WITH fresh AS (%s) SELECT * FROM read_parquet(%s, hive_partitioning=false) "+
			"WHERE event_uid NOT IN (SELECT event_uid FROM fresh) UNION ALL SELECT * FROM fresh",
			query, sqlQuote(eventsPath))
	}
	cp, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	return copyParquet(db, query, eventsPath, string(cp))
}

// resume reads the parquet's checkpoint and, when it belongs to this binary
// and source and the raw's first ZstSize bytes still hash to it, returns it
// with raw positioned at ZstSize and h holding the hash of those bytes.
// Otherwise it returns the zero checkpoint, raw rewound and h empty.
func resume(db *sql.DB, eventsPath string, raw *os.File, src manifest, want checkpoint) (checkpoint, hash.Hash, error) {
	h := sha256.New()
	if cp, ok := readCheckpoint(db, eventsPath); ok &&
		cp.Binary != "" && cp.Binary == want.Binary && cp.Harness == want.Harness &&
		cp.SourceName == want.SourceName && cp.SourceFile == want.SourceFile &&
		cp.ZstSize <= src.ZstSize {
		if _, err := io.CopyN(h, raw, cp.ZstSize); err == nil && hex.EncodeToString(h.Sum(nil)) == cp.ZstSHA256 {
			return cp, h, nil
		}
		h.Reset()
		if _, err := raw.Seek(0, io.SeekStart); err != nil {
			return checkpoint{}, nil, err
		}
	}
	return checkpoint{}, h, nil
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

// restampIfSuperseded re-reads the manifest after the parquet rename: when a
// push landed while this source was parsing, the fresh mtime would make the
// next Run call the parquet current and the appended lines would sit unparsed
// until the source changed again, so pin the mtime to the manifest this run
// parsed and let the next Run see it as stale.
func restampIfSuperseded(mpath, eventsPath string, parsed manifest) error {
	var now manifest
	b, err := os.ReadFile(mpath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &now); err != nil {
		return err
	}
	if now.UpdatedMs == parsed.UpdatedMs && now.ZstSize == parsed.ZstSize {
		return nil
	}
	t := time.UnixMilli(parsed.UpdatedMs)
	return os.Chtimes(eventsPath, t, t)
}

// dedupe keeps, per event_uid, the highest event_version — the in-file
// equivalent of ReplacingMergeTree(event_version); on equal versions the
// later row in file order wins.
func dedupe(events []schema.Event) []schema.Event {
	at := make(map[string]int, len(events))
	out := make([]schema.Event, 0, len(events))
	for i := range events {
		ev := &events[i]
		if j, ok := at[ev.EventUID]; ok {
			if ev.EventVersion >= out[j].EventVersion {
				out[j] = *ev
			}
			continue
		}
		at[ev.EventUID] = len(out)
		out = append(out, *ev)
	}
	return out
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
	return copyParquet(db, query, out, "")
}

func open() (*sql.DB, error) { return sql.Open("duckdb", "") }

// copyParquet writes query to outPath through a temp file and a rename, with
// kv, when set, as the checkpoint metadata, and returns the rows written.
func copyParquet(db *sql.DB, query, outPath, kv string) (int64, error) {
	tmp := outPath + ".tmp"
	opts := "FORMAT parquet, COMPRESSION zstd"
	if kv != "" {
		opts += fmt.Sprintf(", KV_METADATA {%s: %s}", checkpointKey, sqlQuote(kv))
	}
	res, err := db.Exec(fmt.Sprintf("COPY (%s) TO %s (%s)", query, sqlQuote(tmp), opts))
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		os.Remove(tmp)
		return 0, err
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

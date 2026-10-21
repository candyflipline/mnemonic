package lake

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/klauspost/compress/zstd"
)

type fakeParser struct{}

func (fakeParser) Harness() string { return "test-fake" }

func (fakeParser) Parse(r io.Reader, src normalize.SourceMeta, from normalize.State, emit func(*schema.Event) error) (normalize.State, error) {
	sc := bufio.NewScanner(r)
	lineNo := from.LineNo
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			return from, fmt.Errorf("test-fake: line %d: %q", lineNo, line)
		}
		version, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return from, err
		}
		if err := emit(&schema.Event{
			EventUID:          parts[0],
			EventVersion:      version,
			SessionID:         "sess",
			SessionDate:       parts[2][:10],
			SourceName:        src.SourceName,
			Harness:           src.Harness,
			SourceFile:        src.SourceFile,
			SourceLineNo:      lineNo,
			EventTS:           parts[2],
			ContentTypes:      []string{"text"},
			TokenUsageBuckets: map[string]uint64{"input": 3},
		}); err != nil {
			return from, err
		}
	}
	return normalize.State{LineNo: lineNo}, sc.Err()
}

func init() { normalize.Register(fakeParser{}) }

func writeSource(t *testing.T, dataDir, author, machine, harness, srcID string, lines []string) {
	t.Helper()
	key := layout.Key{Author: author, Machine: machine, Harness: harness, SrcID: srcID}
	if err := os.MkdirAll(filepath.Dir(key.Raw(dataDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.OpenFile(key.Raw(dataDir), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	split := (len(lines) + 1) / 2
	for _, half := range [][]string{lines[:split], lines[split:]} {
		if len(half) == 0 {
			continue
		}
		zw, err := zstd.NewWriter(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range half {
			if _, err := zw.Write(append([]byte(line), '\n')); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(key.Raw(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	m := manifest{
		Source: wire.Source{
			SrcID:      srcID,
			Harness:    harness,
			SourceName: "test-project",
			Path:       "/client/" + srcID + ".jsonl",
			Size:       int64(len(lines)),
			HeadHash:   "head",
			UpdatedMs:  time.Now().UnixMilli(),
		},
		ZstSize: info.Size(),
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key.Manifest(dataDir), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countRows(t *testing.T, db *sql.DB, parquet string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM read_parquet(" + sqlQuote(parquet) + ")").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRun(t *testing.T) {
	dataDir := t.TempDir()
	writeSource(t, dataDir, "alice", "chris-mbp", "test-fake", "aaaaaaaaaaaaaaaa", []string{
		"uid-1|1|2024-01-02 03:04:05.000",
		"uid-2|1|2024-01-02 03:04:06.000",
		"uid-3|1|2024-01-02 03:04:07.000",
	})
	writeSource(t, dataDir, "alice", "chris-mbp", "test-fake", "bbbbbbbbbbbbbbbb", []string{
		"uid-4|1|2024-01-02 03:04:08.000",
		"uid-5|1|2024-01-02 03:04:09.000",
	})
	writeSource(t, dataDir, "alice", "chris-mbp", "no-parser", "cccccccccccccccc", []string{
		"uid-x|1|2024-01-02 03:04:10.000",
	})

	st, err := Run(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st != (Stats{Sources: 3, Rewrote: 2, RawOnly: 1, Events: 5}) {
		t.Fatalf("stats = %+v, want {3 2 0 1 0 5}", st)
	}

	db := openDB(t)
	p1 := layout.Key{Author: "alice", Machine: "chris-mbp", Harness: "test-fake", SrcID: "aaaaaaaaaaaaaaaa"}.Events(dataDir)
	p2 := layout.Key{Author: "alice", Machine: "chris-mbp", Harness: "test-fake", SrcID: "bbbbbbbbbbbbbbbb"}.Events(dataDir)
	if n := countRows(t, db, p1); n != 3 {
		t.Fatalf("source 1 rows = %d, want 3", n)
	}
	if n := countRows(t, db, p2); n != 2 {
		t.Fatalf("source 2 rows = %d, want 2", n)
	}

	var uid, author, machine string
	if err := db.QueryRow("SELECT event_uid, author, machine_id FROM read_parquet("+sqlQuote(p1)+") WHERE source_line_no = 1").
		Scan(&uid, &author, &machine); err != nil {
		t.Fatal(err)
	}
	if uid != "uid-1" || author != "alice" || machine != "chris-mbp" {
		t.Fatalf("row = %q/%q/%q, want uid-1/alice/chris-mbp", uid, author, machine)
	}
	var bucket int64
	if err := db.QueryRow("SELECT token_usage_buckets['input'] FROM read_parquet(" + sqlQuote(p1) + ") WHERE event_uid = 'uid-1'").
		Scan(&bucket); err != nil {
		t.Fatal(err)
	}
	if bucket != 3 {
		t.Fatalf("token_usage_buckets['input'] = %d, want 3", bucket)
	}

	rows, err := db.Query("DESCRIBE SELECT * FROM read_parquet(" + sqlQuote(p1) + ", hive_partitioning=false)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type col struct{ name, typ string }
	var got []col
	for rows.Next() {
		var c col
		var null, key, def, extra any
		if err := rows.Scan(&c.name, &c.typ, &null, &key, &def, &extra); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := make([]col, 0, len(schema.EventColumns)+3)
	for _, c := range schema.EventColumns {
		want = append(want, col{c.Name, c.DuckDB})
	}
	want = append(want, col{"author", "VARCHAR"}, col{"machine_id", "VARCHAR"}, col{"ingested_at", "TIMESTAMP"})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parquet schema = %v, want %v", got, want)
	}

	st2, err := Run(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st2 != (Stats{Sources: 3, Unchanged: 2, RawOnly: 1}) {
		t.Fatalf("second run stats = %+v, want {3 0 2 1 0 0}", st2)
	}
}

func TestRunKeepsHighestEventVersion(t *testing.T) {
	dataDir := t.TempDir()
	writeSource(t, dataDir, "alice", "chris-mbp", "test-fake", "dddddddddddddddd", []string{
		"uid-a|1|2024-01-02 03:04:05.000",
		"uid-b|5|2024-01-02 03:04:06.000",
		"uid-a|9|2024-01-02 03:04:07.000",
	})

	st, err := Run(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Events != 2 {
		t.Fatalf("events = %d, want 2", st.Events)
	}
	p := layout.Key{Author: "alice", Machine: "chris-mbp", Harness: "test-fake", SrcID: "dddddddddddddddd"}.Events(dataDir)
	var version int64
	if err := openDB(t).QueryRow("SELECT event_version FROM read_parquet(" + sqlQuote(p) + ") WHERE event_uid = 'uid-a'").
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 9 {
		t.Fatalf("uid-a event_version = %d, want 9", version)
	}
}

func TestImportClickHouse(t *testing.T) {
	dataDir := t.TempDir()
	writeSource(t, dataDir, "alice", "chris-mbp", "test-fake", "eeeeeeeeeeeeeeee", []string{
		"uid-live|1|2024-01-02 03:04:05.000",
	})
	if st, err := Run(dataDir); err != nil || st.Events != 1 {
		t.Fatalf("live run: %+v, %v", st, err)
	}

	db := openDB(t)
	exportDir := t.TempDir()
	exportEvents := []schema.Event{
		{EventUID: "uid-live", EventVersion: 1, SessionDate: "2024-01-02", Harness: "test-fake",
			SourceFile: "/client/eeeeeeeeeeeeeeee.jsonl", EventTS: "2024-01-02 03:04:05.000"},
		{EventUID: "uid-old", EventVersion: 1, SessionDate: "2024-01-02", Harness: "test-fake",
			SourceFile: "/client/gone.jsonl", EventTS: "2024-01-02 03:04:06.000"},
	}
	ndjson, err := os.CreateTemp(exportDir, "export-*.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(ndjson.Name())
	enc := json.NewEncoder(ndjson)
	for i := range exportEvents {
		if err := enc.Encode(&exportEvents[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := ndjson.Close(); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(exportDir, "events-202401.parquet")
	query := "SELECT " + selectList("alice", "chris-mbp", "2024-01-01 00:00:00.000") +
		" FROM read_json(" + sqlQuote(ndjson.Name()) + ", format='newline_delimited', columns=" + columnTypes() + ")"
	if _, err := copyParquet(db, query, exportPath, "", ""); err != nil {
		t.Fatal(err)
	}

	st, err := ImportClickHouse(dataDir, exportDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if st != (Stats{Sources: 1, Rewrote: 1, Events: 1}) {
		t.Fatalf("stats = %+v, want {1 1 0 0 0 1}", st)
	}
	out := filepath.Join(dataDir, "events", "author=alice", "machine=archive-ch", "harness=test-fake", "ch-events-202401.parquet")
	if n := countRows(t, db, out); n != 1 {
		t.Fatalf("archive rows = %d, want 1", n)
	}
	var sourceFile, machineID, auth string
	var ingested time.Time
	if err := db.QueryRow("SELECT source_file, machine_id, author, ingested_at FROM read_parquet("+sqlQuote(out)+")").
		Scan(&sourceFile, &machineID, &auth, &ingested); err != nil {
		t.Fatal(err)
	}
	if sourceFile != "/client/gone.jsonl" || machineID != "archive-ch" || auth != "alice" || !ingested.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("archive row = %q/%q/%q/%v", sourceFile, machineID, auth, ingested)
	}

	st2, err := ImportClickHouse(dataDir, exportDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if st2.Events != 1 {
		t.Fatalf("rerun events = %d, want 1", st2.Events)
	}
	if n := countRows(t, db, out); n != 1 {
		t.Fatalf("rerun archive rows = %d, want 1", n)
	}
}

func TestRunParsesAppendStampedBeforeLakeWrite(t *testing.T) {
	dataDir := t.TempDir()
	key := layout.Key{Author: "alice", Machine: "chris-mbp", Harness: "test-fake", SrcID: "eeeeeeeeeeeeeeee"}
	writeSource(t, dataDir, key.Author, key.Machine, key.Harness, key.SrcID, []string{
		"uid-1|1|2024-01-02 03:04:05.000",
	})
	setManifest := func(f func(*manifest)) {
		var m manifest
		b, err := os.ReadFile(key.Manifest(dataDir))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		f(&m)
		if b, err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(key.Manifest(dataDir), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pushedAt := time.Now().Add(-time.Second).UnixMilli()
	setManifest(func(m *manifest) { m.UpdatedMs = pushedAt })
	if st, err := Run(dataDir); err != nil || st.Rewrote != 1 {
		t.Fatalf("first run: %+v %v", st, err)
	}

	raw, err := os.OpenFile(key.Raw(dataDir), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	zw, _ := zstd.NewWriter(raw)
	zw.Write([]byte("uid-2|1|2024-01-02 03:04:06.000\n"))
	zw.Close()
	info, _ := raw.Stat()
	raw.Close()
	setManifest(func(m *manifest) { m.ZstSize, m.UpdatedMs = info.Size(), pushedAt+1 })

	st, err := Run(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rewrote != 1 || st.Unchanged != 0 || st.Events != 1 {
		t.Fatalf("append pushed 1 ms after the parsed push: %+v, want it parsed", st)
	}
}

func TestRunReparsesIdleSourceOfOlderParser(t *testing.T) {
	dataDir := t.TempDir()
	key := layout.Key{Author: "alice", Machine: "chris-mbp", Harness: "test-fake", SrcID: "ffffffffffffffff"}
	writeSource(t, dataDir, key.Author, key.Machine, key.Harness, key.SrcID, []string{
		"uid-1|1|2024-01-02 03:04:05.000",
		"uid-2|1|2024-01-02 03:04:06.000",
	})
	if st, err := Run(dataDir); err != nil || st.Rewrote != 1 {
		t.Fatalf("first run: %+v %v", st, err)
	}

	db := openDB(t)
	base := key.Events(dataDir)
	cp, ok := readCheckpoint(db, base)
	if !ok {
		t.Fatal("no checkpoint on the first run's file")
	}
	cp.Parser = normalize.Version - 1
	kv, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copyParquet(db, "SELECT * FROM "+lakeRows(base), base, string(kv), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dataDir, "events", ".parser-version")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	st, err := Run(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st != (Stats{Sources: 1, Rewrote: 1, Events: 2}) {
		t.Fatalf("run after a parser bump, no new push: %+v, want {1 1 0 0 0 2}", st)
	}
	if cp, _ := readCheckpoint(db, base); cp.Parser != normalize.Version {
		t.Fatalf("checkpoint parser = %d, want %d", cp.Parser, normalize.Version)
	}
	if st, err := Run(dataDir); err != nil || st != (Stats{Sources: 1, Unchanged: 1}) {
		t.Fatalf("further run: %+v %v, want {1 0 1 0 0 0}", st, err)
	}
}

package index

import (
	"bytes"
	"context"
	"encoding/base64"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/klauspost/compress/zstd"
	"github.com/parquet-go/parquet-go"
)

// The archive kind's one committed test. A ch-* parquet with three sessions
// ingests as per-session members beside a raw source of one of them; the raw
// copy wins the uid the two share; an unchanged file rewrites nothing; a
// changed file re-parses all of its members in one pass; an append to the raw
// source rebuilds only the shared session's component, not the file's; and a
// 70-session file exercises the bounded open-log LRU, including a reopen.
func TestArchiveKind(t *testing.T) {
	ctx := context.Background()
	const (
		archS1   = "11111111-1111-4111-8111-111111111111" // shared with the raw source
		archS2   = "22222222-2222-4222-8222-222222222222" // archive-only
		archS3   = "33333333-3333-4333-8333-333333333333" // archive-only
		chKey    = "a__archive-ch__claude-code__ch-export"
		rawSrcID = "a__m__claude-code__s1"
	)
	at := func(min int) (string, int64) {
		ts := time.Date(2026, 3, 1, 10, min, 0, 0, time.UTC)
		return ts.Format("2006-01-02T15:04:05Z"), ts.UnixMicro()
	}
	claudeLine := func(uuid, ts, content string) []byte {
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/arch","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
			archS1, uuid, ts, content))
	}
	archRow := func(uid, sid, recordTS string, eventUS int64, text string, ver uint64) chRow {
		return chRow{
			EventUID: uid, SessionID: sid, RecordTS: recordTS, EventTS: eventUS,
			SourceName: "claude-code", Harness: "claude-code",
			SourceFile: "/orig/" + sid + ".jsonl",
			// The columns search's default event-type mask reads; the real
			// files carry them filled, as the raw parse stamps them.
			EventKind: "message", ActorKind: "user", PayloadType: "message",
			TextContent:  text, TextPreview: text,
			EventVersion: ver, Author: "a", MachineID: "archive-ch",
		}
	}

	root, store := t.TempDir(), t.TempDir()
	// The raw source: two user lines of the shared session, the second's uid
	// the archive file copies.
	ts0, _ := at(0)
	ts1, _ := at(1)
	raw := append(claudeLine("raw-1", ts0, "rawonly alpha question"), claudeLine("raw-2", ts1, "shared beta question")...)
	writeRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", raw, 1)
	overlapUID := rawUIDs(t, raw)[1]

	chPath := filepath.Join(root, "events", "author=a", "machine=archive-ch", "harness=claude-code", "ch-export.parquet")
	ts5, us5 := at(5)
	ts6, us6 := at(6)
	ts7, us7 := at(7)
	ts8, us8 := at(8)
	rows := []chRow{
		archRow(overlapUID, archS1, ts5, us5, "shared beta ARCHIVE COPY", 100),
		archRow("arch-uid-s1", archS1, ts6, us6, "archshared gamma", 101),
	}
	r := archRow("arch-uid-s2a", archS2, ts7, us7, "archtwo otter", 102)
	r.TokenUsageBuckets = map[string]uint64{"input": 7}
	r.TokenUsageNativeUnits = map[string]float64{"tokens": 1.5}
	rows = append(rows, r,
		archRow("arch-uid-s2b", archS2, ts8, us8, "archtwo pelican", 103))
	ts9, us9 := at(9)
	rows = append(rows, archRow("arch-uid-s3", archS3, ts9, us9, "archthree zebra", 104))
	writeChParquet(t, chPath, rows)

	// First ingest: raw and the archive file are both Fresh.
	stats, err := Ingest(root, store, time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{0, 0, 2, 0}) {
		t.Fatalf("first ingest: %v, want [0 0 2 0]", stats)
	}
	// The file checkpoint names its three member sessions.
	fc, ok := readCkpt(store, chKey)
	if !ok {
		t.Fatal("no archive file checkpoint")
	}
	if fc.Kind != KindArchive || !slices.Equal(fc.Sessions, []string{archS1, archS2, archS3}) || fc.Rows != 5 {
		t.Fatalf("file checkpoint: kind %v sessions %v rows %d", fc.Kind, fc.Sessions, fc.Rows)
	}
	// Per-session members carry the parent file's identity.
	mc, ok := readCkpt(store, chKey+"~"+archS1)
	if !ok {
		t.Fatal("no member checkpoint for the shared session")
	}
	if mc.Kind != KindArchive || !slices.Equal(mc.Sessions, []string{archS1}) || mc.Rows != 2 ||
		mc.ZstSize != fc.ZstSize || mc.WindowSHA != fc.WindowSHA || mc.Author != "a" || mc.Gen != 0 {
		t.Fatalf("shared member: %+v", mc.parsed)
	}
	for _, c := range []struct{ sid string; rows int64 }{{archS2, 2}, {archS3, 1}} {
		m, ok := readCkpt(store, chKey+"~"+c.sid)
		if !ok || m.Rows != c.rows {
			t.Fatalf("member %s: rows %d ok %v, want %d", c.sid, m.Rows, ok, c.rows)
		}
	}

	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	if docs := docsTotal(db); docs != 6 {
		t.Fatalf("docs %d, want 6 (3+2+1 winners)", docs)
	}
	// The raw copy wins the shared uid even though the archive copy carries a
	// later record_ts and event_version.
	w := findRow(t, db, overlapUID)
	if w.Text != "shared beta question" || w.SourceFile != "/p/s1.jsonl" {
		t.Fatalf("shared uid winner: text %q source %q", w.Text, w.SourceFile)
	}
	if a := findRow(t, db, "arch-uid-s1"); a.Text != "archshared gamma" || a.SourceFile != "/orig/"+archS1+".jsonl" {
		t.Fatalf("archive-only uid of the shared session: %+v", a)
	}
	// The MAP columns decode into the row's JSON the way rowFrom marshals them.
	if b := findRow(t, db, "arch-uid-s2a"); b.Buckets != `{"input":7}` || b.Native != `{"tokens":1.5}` {
		t.Fatalf("map columns: buckets %q native %q", b.Buckets, b.Native)
	}
	// Archive-only sessions are listable.
	data, _, err := Call(ctx, db, "list_sessions",
		json.RawMessage(`{"start_datetime":"2026-03-01T00:00:00Z","end_datetime":"2026-03-02T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ls struct {
		ResultCount int `json:"result_count"`
		Sessions    []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &ls); err != nil {
		t.Fatal(err)
	}
	if ls.ResultCount != 3 {
		t.Fatalf("list_sessions count %d, want 3", ls.ResultCount)
	}
	// Tool ids encode the session id: the three raw ids appear base64-encoded.
	for _, sid := range []string{archS1, archS2, archS3} {
		if !strings.Contains(string(data), base64.StdEncoding.EncodeToString([]byte(sid))) {
			t.Fatalf("list_sessions misses %s: %s", sid, data)
		}
	}
	// and searchable.
	data, _, err = Call(ctx, db, "search_sessions", json.RawMessage(`{"query":"archthree"}`))
	if err != nil {
		t.Fatal(err)
	}
	var sr struct {
		ResultCount int `json:"result_count"`
		Results     []struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.ResultCount != 1 || !strings.Contains(sr.Results[0].Session.ID,
		base64.StdEncoding.EncodeToString([]byte(archS3))) {
		t.Fatalf("search archthree: %s", data)
	}
	// Session authors are the path author, archive rows included.
	for _, sid := range []string{archS1, archS2, archS3} {
		if got := db.sessionAuthors(sid); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("authors of %s: %v, want [a]", sid, got)
		}
	}
	// CURRENT's sources rows exclude the archive file and its members.
	data, _, err = Call(ctx, db, "get_ingest_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Current *struct {
			FilesWatched int `json:"files_watched"`
			Progress     *struct {
				FilesTotal int `json:"files_total"`
			} `json:"progress"`
		} `json:"current"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.Current == nil || st.Current.FilesWatched != 1 || st.Current.Progress.FilesTotal != 1 {
		t.Fatalf("ingest status counts a non-source: %s", data)
	}
	db.Close()

	// Re-ingest with nothing changed rewrites nothing.
	before := hashTree(t, store)
	if stats, err = Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{2, 0, 0, 0}) {
		t.Fatalf("unchanged re-ingest: %v, want [2 0 0 0]", stats)
	}
	if after := hashTree(t, store); after != before {
		t.Fatal("unchanged re-ingest rewrote the store")
	}

	// A changed file re-parses all of its members in one pass.
	ts10, us10 := at(10)
	rows = append(rows, archRow("arch-uid-s3b", archS3, ts10, us10, "archthree crane", 105))
	writeChParquet(t, chPath, rows)
	if stats, err = Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{1, 0, 0, 1}) {
		t.Fatalf("changed-file ingest: %v, want [1 0 0 1]", stats)
	}
	if fc, ok = readCkpt(store, chKey); !ok || fc.Rows != 6 {
		t.Fatalf("file checkpoint after reparse: rows %d ok %v, want 6", fc.Rows, ok)
	}
	m3, ok := readCkpt(store, chKey+"~"+archS3)
	if !ok || m3.Rows != 2 || m3.Gen != 1 {
		t.Fatalf("member %s after reparse: rows %d gen %d, want 2 rows gen 1", archS3, m3.Rows, m3.Gen)
	}
	if mc, _ = readCkpt(store, chKey+"~"+archS1); mc.Rows != 2 || mc.Gen != 1 {
		t.Fatalf("shared member after reparse: rows %d gen %d, want 2 rows gen 1", mc.Rows, mc.Gen)
	}
	if db, err = OpenDB(store); err != nil {
		t.Fatal(err)
	}
	if docs := docsTotal(db); docs != 7 {
		t.Fatalf("docs after reparse %d, want 7", docs)
	}
	db.Close()

	// An append to the raw source rebuilds only the shared session's
	// component: the hot part holds that session's winners (12 raw uids plus
	// the archive member's own), not the archive file's rows.
	ten := make([]byte, 0, 10*200)
	for i := 3; i <= 12; i++ {
		tsi, _ := at(20 + i)
		ten = append(ten, claudeLine(fmt.Sprintf("raw-%d", i), tsi, fmt.Sprintf("appended %02d question", i))...)
	}
	zstPath := filepath.Join(root, "raw", "author=a", "machine=m", "harness=claude-code", "s1.jsonl.zst")
	zf, err := os.OpenFile(zstPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := zstd.NewWriter(nil)
	_, err = zf.Write(enc.EncodeAll(ten, nil))
	zf.Close()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(zstPath)
	if err != nil {
		t.Fatal(err)
	}
	writeRawManifest(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl",
		int64(len(raw))+int64(len(ten)), fi.Size(), time.Now().UnixMilli())
	if stats, err = Ingest(root, store, 15*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{1, 1, 0, 0}) {
		t.Fatalf("append ingest: %v, want [1 1 0 0]", stats)
	}
	if rc, _ := readCkpt(store, rawSrcID); rc.Rows != 12 {
		t.Fatalf("raw rows %d, want 12", rc.Rows)
	}
	mf, err := readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Hot) != 1 {
		t.Fatalf("hot parts %d, want 1 (the shared session's component)", len(mf.Hot))
	}
	if db, err = OpenDB(store); err != nil {
		t.Fatal(err)
	}
	hp, ok := db.part(archS1)
	if !ok {
		t.Fatal("no part holds the shared session")
	}
	if hp.meta.Docs != 13 {
		t.Fatalf("hot part docs %d, want 13, not the file's rows", hp.meta.Docs)
	}
	if docs := docsTotal(db); docs != 17 {
		t.Fatalf("docs after append %d, want 17", docs)
	}
	if w = findRow(t, db, overlapUID); w.Text != "shared beta question" {
		t.Fatalf("shared uid winner after append: %q", w.Text)
	}
	db.Close()

	// A 70-row file over 69 sessions drives the open-log LRU past its limit
	// and reopens the first session's logs for its second row.
	root2, store2 := t.TempDir(), t.TempDir()
	bigPath := filepath.Join(root2, "events", "author=a", "machine=archive-ch", "harness=other", "ch-big.parquet")
	const chBigKey = "a__archive-ch__other__ch-big"
	base := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	sid1 := fmt.Sprintf("eeeeeeee-eeee-4eee-8eee-%012d", 1)
	var big []chRow
	for i := 1; i <= 69; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		big = append(big, archRow(fmt.Sprintf("ev-uid-%d", i),
			fmt.Sprintf("eeeeeeee-eeee-4eee-8eee-%012d", i),
			ts.Format("2006-01-02T15:04:05Z"), ts.UnixMicro(),
			fmt.Sprintf("evword%d quail", i), uint64(200+i)))
	}
	ts70 := base.Add(70 * time.Minute)
	big = append(big, archRow("ev-uid-1b", sid1, ts70.Format("2006-01-02T15:04:05Z"), ts70.UnixMicro(), "evword1 second quail", 300))
	writeChParquet(t, bigPath, big)
	if stats, err = Ingest(root2, store2, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{0, 0, 1, 0}) {
		t.Fatalf("big-file ingest: %v, want [0 0 1 0]", stats)
	}
	if fc, ok = readCkpt(store2, chBigKey); !ok || fc.Rows != 70 || len(fc.Sessions) != 69 {
		t.Fatalf("big file checkpoint: rows %d sessions %d, want 70 and 69", fc.Rows, len(fc.Sessions))
	}
	m1, ok := readCkpt(store2, chBigKey+"~"+sid1)
	if !ok || m1.Rows != 2 || m1.Gen != 0 {
		t.Fatalf("reopened member: rows %d gen %d, want 2 rows gen 0", m1.Rows, m1.Gen)
	}
	for i := 1; i <= 69; i++ {
		if _, ok := readCkpt(store2, fmt.Sprintf("%s~eeeeeeee-eeee-4eee-8eee-%012d", chBigKey, i)); !ok {
			t.Fatalf("no member checkpoint for session %d", i)
		}
	}
	if db, err = OpenDB(store2); err != nil {
		t.Fatal(err)
	}
	if docs := docsTotal(db); docs != 70 {
		t.Fatalf("big-file docs %d, want 70 (the reopen must not lose rows)", docs)
	}
	data, _, err = Call(ctx, db, "search_sessions", json.RawMessage(`{"query":"evword17"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), base64.StdEncoding.EncodeToString(
		[]byte("eeeeeeee-eeee-4eee-8eee-000000000017"))) {
		t.Fatalf("search evword17: %s", data)
	}
	db.Close()
	before2 := hashTree(t, store2)
	if stats, err = Ingest(root2, store2, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if stats != (IngestStats{1, 0, 0, 0}) || hashTree(t, store2) != before2 {
		t.Fatalf("big-file re-ingest: %v, want [1 0 0 0] and no writes", stats)
	}
}

// claudeRaw writes one raw source's zst and manifest the way push would.
func writeRawSource(t *testing.T, root, author, machine, harness, srcID, path string, plain []byte, updatedMs int64) {
	t.Helper()
	enc, _ := zstd.NewWriter(nil)
	writeRawManifest(t, root, author, machine, harness, srcID, path, int64(len(plain)),
		int64(len(enc.EncodeAll(plain, nil))), updatedMs)
	zst := enc.EncodeAll(plain, nil)
	dir := filepath.Join(root, "raw", "author="+author, "machine="+machine, "harness="+harness)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, srcID+".jsonl.zst"), zst, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRawManifest (re)writes the manifest: an append rewrites it in place
// with the new sizes, as push does.
func writeRawManifest(t *testing.T, root, author, machine, harness, srcID, path string, size, zstSize, updatedMs int64) {
	t.Helper()
	dir := filepath.Join(root, "raw", "author="+author, "machine="+machine, "harness="+harness)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{"src_id": srcID, "harness": harness, "source_name": harness,
		"path": path, "size": size, "updated_ms": updatedMs, "zst_size": zstSize})
	if err := os.WriteFile(filepath.Join(dir, srcID+".json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeChParquet writes the ch file the test rows through parquet-go's
// writer; no writer exists outside tests.
func writeChParquet(t *testing.T, path string, rows []chRow) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := parquet.Write(f, rows); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// rawUIDs parses plain lines the way ingest will, so the test can plant a
// raw row's uid in the archive file.
func rawUIDs(t *testing.T, plain []byte) []string {
	t.Helper()
	parser, ok := normalize.For("claude-code")
	if !ok {
		t.Fatal("no claude-code parser")
	}
	var uids []string
	_, err := parser.Parse(bytes.NewReader(plain),
		normalize.SourceMeta{SourceName: "claude-code", Harness: "claude-code", SourceFile: "/p/s1.jsonl"},
		normalize.State{}, func(ev *schema.Event) error {
			uids = append(uids, ev.EventUID)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 2 {
		t.Fatalf("raw lines yielded %d events, want 2", len(uids))
	}
	return uids
}

// findRow is the one row of db whose uid is uid, failing when there is none
// or more than one.
func findRow(t *testing.T, db *DB, uid string) Row {
	t.Helper()
	var found []Row
	for _, p := range db.parts {
		for d := 0; d < p.meta.Docs; d++ {
			r, err := p.row(d)
			if err != nil {
				t.Fatal(err)
			}
			if r.UID == uid {
				found = append(found, *r)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("uid %s: %d rows, want 1", uid, len(found))
	}
	return found[0]
}

func docsTotal(db *DB) int {
	n := 0
	for _, p := range db.parts {
		n += p.meta.Docs
	}
	return n
}

// hashTree is a digest of every path and file body under dir, so a test can
// prove an ingest wrote nothing.
func hashTree(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	var paths []string
	var dirs map[string]bool
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel != "." {
			paths = append(paths, rel)
		}
		if d.IsDir() {
			if dirs == nil {
				dirs = map[string]bool{}
			}
			dirs[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	for _, rel := range paths {
		h.Write([]byte(rel))
		if dirs[rel] {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%d:", len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

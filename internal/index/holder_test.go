package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/klauspost/compress/zstd"
)

func cmpPartDirs(t *testing.T, want, got string) {
	t.Helper()
	var wantFiles, gotFiles []string
	for _, dir := range []struct {
		root string
		out  *[]string
	}{{want, &wantFiles}, {got, &gotFiles}} {
		err := filepath.WalkDir(dir.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				*dir.out = append(*dir.out, filepath.Base(p))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(wantFiles)
	slices.Sort(gotFiles)
	if !slices.Equal(wantFiles, gotFiles) {
		t.Fatalf("part files %v, want %v", gotFiles, wantFiles)
	}
	for _, name := range wantFiles {
		a, err := os.ReadFile(filepath.Join(want, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(got, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: %d bytes, holder build wrote %d different ones", name, len(a), len(b))
		}
	}
}

func memberCks(t *testing.T, store, id string) []ckpt {
	t.Helper()
	d, ok := readBase(store, id)
	if !ok {
		t.Fatalf("%s: no member list", id)
	}
	var cks []ckpt
	for _, m := range d.Members {
		c, ok := readCkpt(store, m.Key)
		if !ok {
			t.Fatalf("%s: member %s has no checkpoint", id, m.Key)
		}
		cks = append(cks, c)
	}
	return cks
}

func partHolding(t *testing.T, store, substr string) string {
	t.Helper()
	mf, err := readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range mf.Parts {
		for _, m := range memberCks(t, store, id) {
			if strings.Contains(m.Key, substr) {
				return id
			}
		}
	}
	t.Fatalf("no part holds a member matching %s", substr)
	return ""
}

func buildBoth(t *testing.T, store string, cks []ckpt, prev manifestFile) {
	t.Helper()
	fp := fingerprint(cks)
	tmp := t.TempDir()
	logsDir := filepath.Join(tmp, "logs")
	if err := buildIndex(store, cks, fp, logsDir, nil); err != nil {
		t.Fatal(err)
	}
	holderDir := filepath.Join(tmp, "holders")
	if err := buildIndex(store, cks, fp, holderDir, prevPartsMap(store, prev)); err != nil {
		t.Fatal(err)
	}
	cmpPartDirs(t, logsDir, holderDir)
}

func TestHolderPartBytesMatchTheLogsPath(t *testing.T) {
	const sid = "5c666c01-d38e-4658-8650-854ffb5b626e"
	at := func(min int) (string, int64) {
		ts := time.Date(2026, 3, 2, 10, min, 0, 0, time.UTC)
		return ts.Format("2006-01-02T15:04:05Z"), ts.UnixMicro()
	}
	claudeLine := func(session, uuid, ts, content string) []byte {
		return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/win","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
			session, uuid, ts, content))
	}
	var raw []byte
	for i := 1; i <= 24; i++ {
		ts, _ := at(i)
		text := fmt.Sprintf("filler %02d question", i)
		if i == 2 || i == 5 {
			text = fmt.Sprintf("shared %02d question", i)
		}
		raw = append(raw, claudeLine(sid, fmt.Sprintf("raw-%d", i), ts, text)...)
	}
	uids := sourceUIDs(t, raw, 24)
	archRow := func(session, uid, recordTS string, eventUS int64, text string, ver uint64) chRow {
		return chRow{
			EventUID: uid, SessionID: session, RecordTS: recordTS, EventTS: eventUS,
			SourceName: "claude-code", Harness: "claude-code",
			SourceFile: "/orig/" + session + ".jsonl",
			EventKind:  "message", ActorKind: "user", PayloadType: "message",
			TextContent: text, TextPreview: text,
			EventVersion: ver, MachineID: "archive-ch",
		}
	}
	plant := func(root, author string, rows []chRow, name string) {
		writeChParquet(t, filepath.Join(root, "events", "author="+author,
			"machine=archive-ch", "harness=claude-code", name), rows)
	}

	root, store := t.TempDir(), t.TempDir()
	writeRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", raw, 1)
	if _, err := Ingest(root, store, 15*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	ts30, us30 := at(30)
	ts31, us31 := at(31)
	plant(root, "b", []chRow{
		archRow(sid, uids[1], ts30, us30, "shared 02 ARCHIVE", 100),
		archRow(sid, uids[4], ts31, us31, "shared 05 ARCHIVE", 101),
		archRow(sid, "arch-only-1", ts30, us30, "archonly one", 102),
		archRow(sid, "arch-only-2", ts31, us31, "archonly two", 103),
	}, "ch-b.parquet")
	plant(root, "f", []chRow{
		archRow(sid, uids[0], ts30, us30, "shared 01 LOSER", 100),
		archRow(sid, uids[3], ts31, us31, "shared 04 LOSER", 101),
	}, "ch-f.parquet")
	var grown []byte
	enc := zstdEncoder(t)
	grown = enc.EncodeAll(raw, nil)
	for i := 25; i <= 26; i++ {
		ts, _ := at(40 + i)
		line := claudeLine(sid, fmt.Sprintf("raw-%d", i), ts, fmt.Sprintf("tail %02d question", i))
		grown = enc.EncodeAll(line, grown)
		raw = append(raw, line...)
	}
	writeRawZst(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", raw, grown, 2)
	if _, err := Ingest(root, store, 15*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	mf, err := readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(mf.Hot) != 1 {
		t.Fatalf("%d hot parts, want the one delta", len(mf.Hot))
	}
	if _, isDelta, err := readDelta(partDir(store, mf.Hot[0])); err != nil || !isDelta {
		t.Fatalf("the append did not hold a delta: isDelta %v err %v", isDelta, err)
	}
	dp, err := OpenPartDir(partDir(store, mf.Hot[0]))
	if err != nil {
		t.Fatal(err)
	}
	if authors := dp.authSection()[sid]; !slices.Equal(authors, []string{"a", "b", "f"}) {
		t.Fatalf("delta part authors of the shared session %v, want a, b and the loser-only f", authors)
	}
	if dp.meta.Docs != 28 {
		t.Fatalf("delta part holds %d docs, want s1's 26 winners and b's two own", dp.meta.Docs)
	}
	dp.Close()
	cks := memberCks(t, store, mf.Hot[0])
	buildBoth(t, store, cks, mf)
	if d, _, err := readDelta(partDir(store, mf.Hot[0])); err != nil {
		t.Fatal(err)
	} else if got := len(d.Members); got != 3 {
		t.Fatalf("delta holds %d members, want 3", got)
	}

	shard := func(s string) uint32 {
		h := fnv.New32a()
		h.Write([]byte(s))
		return h.Sum32() % partShards
	}
	s2 := "00000000-0000-4000-8000-000000000001"
	s4 := "00000000-0000-4000-8000-000000000004"
	for i := 5; shard(s4) != shard(s2); i++ {
		s4 = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	cline := func(uuid, ts, content string) []byte {
		return claudeLine(s2, uuid, ts, content)
	}
	dline := func(uuid, ts, content string) []byte {
		return claudeLine(s4, uuid, ts, content)
	}
	ts1, _ := at(50)
	ts2, _ := at(51)
	root2, store2 := t.TempDir(), t.TempDir()
	writeRawSource(t, root2, "a", "m", "claude-code", "c1", "/p/c1.jsonl",
		append(cline("c-1", ts1, "c one question"), cline("c-2", ts2, "c two question")...), 1000)
	if _, err := Ingest(root2, store2, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	shardID := partHolding(t, store2, "__c1")
	if slices.Contains(mustManifest(t, store2).Hot, shardID) {
		t.Fatal("c's settled part is hot")
	}
	writeRawSource(t, root2, "a", "m", "claude-code", "d1", "/p/d1.jsonl",
		append(dline("d-1", ts1, "d one question"), dline("d-2", ts2, "d two question")...), 2000)
	if _, err := Ingest(root2, store2, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	rewritten := partHolding(t, store2, "__c1")
	if rewritten == shardID {
		t.Fatal("the shard was not rewritten")
	}
	cks2 := memberCks(t, store2, rewritten)
	if len(cks2) != 2 {
		t.Fatalf("shard holds %d members, want c and d", len(cks2))
	}
	buildBoth(t, store2, cks2, mustManifest(t, store2))
}

func TestMemberRowsReadsEachHolderFrameOnce(t *testing.T) {
	const n = 2400
	root, store := t.TempDir(), t.TempDir()
	sid := "6f0d9a52-1c3b-4e5f-8a7b-9d2e4f6a8b0c"
	var raw []byte
	for i := 1; i <= n; i++ {
		raw = append(raw, []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/win","uuid":"row-%d","timestamp":"2026-03-02T10:%02d:%02dZ","message":{"role":"user","content":"member rows frame cache filler %d shared question"}}`+"\n",
			sid, i, i/60, i%60, i))...)
	}
	writeRawSource(t, root, "a", "m", "claude-code", "big", "/p/big.jsonl", raw, 1)
	if _, err := Ingest(root, store, 15*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	mf := mustManifest(t, store)
	if len(mf.Parts) != 1 || len(mf.Hot) != 1 {
		t.Fatalf("%d parts (%d hot), want the one hot image", len(mf.Parts), len(mf.Hot))
	}
	cks := memberCks(t, store, mf.Parts[0])
	if len(cks) != 1 {
		t.Fatalf("%d members, want 1", len(cks))
	}
	c := cks[0]
	holder, err := OpenPartDir(partDir(store, mf.Parts[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	from, ok := memberFrom(c, holder)
	if !ok {
		t.Fatalf("%s: the hot part does not cover the member", c.Key)
	}
	rowsEqual := func(a, b *Row) bool {
		x, y := *a, *b
		x.HeavyFrame, x.HeavyIdx, y.HeavyFrame, y.HeavyIdx = 0, 0, 0, 0
		return reflect.DeepEqual(x, y)
	}
	holderRun := func() ([]built, error) { return memberRows(store, 0, c, holder, from) }
	logsRun := func() ([]built, error) { return memberRows(store, 0, c, nil, 0) }
	logsRows, err := logsRun()
	if err != nil {
		t.Fatal(err)
	}
	if len(logsRows) < 2000 {
		t.Fatalf("logs read holds %d rows, want the member's >= 2000", len(logsRows))
	}
	holderRows, err := holderRun()
	if err != nil {
		t.Fatal(err)
	}
	if len(holderRows) != len(logsRows) {
		t.Fatalf("holder read holds %d rows, logs read %d", len(holderRows), len(logsRows))
	}
	for i := range logsRows {
		if !rowsEqual(holderRows[i].row, logsRows[i].row) {
			t.Fatalf("row %d differs: holder uid %q, logs uid %q", i, holderRows[i].row.UID, logsRows[i].row.UID)
		}
	}
	allocOf := func(fn func() error) uint64 {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if err := fn(); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	holderAlloc := allocOf(func() error { _, err := holderRun(); return err })
	logsAlloc := allocOf(func() error { _, err := logsRun(); return err })
	if holderAlloc > 2*logsAlloc {
		t.Fatalf("holder read allocated %d B, %.1fx the logs read's %d B; want <= 2x", holderAlloc, float64(holderAlloc)/float64(logsAlloc), logsAlloc)
	}
	t.Logf("holder read %d B, logs read %d B (%.2fx), %d rows", holderAlloc, logsAlloc, float64(holderAlloc)/float64(logsAlloc), len(logsRows))
}

func writeRawZst(t *testing.T, root, author, machine, harness, srcID, path string, plain, zst []byte, updatedMs int64) {
	t.Helper()
	dir := filepath.Join(root, "raw", "author="+author, "machine="+machine, "harness="+harness)
	m, _ := json.Marshal(map[string]any{"src_id": srcID, "harness": harness, "source_name": harness,
		"path": path, "size": len(plain), "updated_ms": updatedMs, "zst_size": len(zst)})
	if err := os.WriteFile(filepath.Join(dir, srcID+".json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, srcID+".jsonl.zst"), zst, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sourceUIDs(t *testing.T, plain []byte, want int) []string {
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
	if len(uids) != want {
		t.Fatalf("raw lines yielded %d events, want %d", len(uids), want)
	}
	return uids
}

func zstdEncoder(t *testing.T) *zstd.Encoder {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func mustManifest(t *testing.T, store string) manifestFile {
	t.Helper()
	mf, err := readManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

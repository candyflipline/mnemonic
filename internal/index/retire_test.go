package index

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	retireShared = "44444444-4444-4444-8444-444444444444"
	retireOnly   = "55555555-5555-4555-8555-555555555555"
)

func retireLine(uuid string, minute int, content string) []byte {
	ts := time.Date(2026, 3, 1, 10, minute, 0, 0, time.UTC).Format("2006-01-02T15:04:05Z")
	return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/p","uuid":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
		retireShared, uuid, ts, content))
}

// retireInputs plants a raw source and a ch file sharing one session: the raw
// copies win both shared uids at equal event version, so the archive copies
// live only in the archive member's rows. It returns the two raw uids.
func retireInputs(t *testing.T, root string) []string {
	t.Helper()
	raw := append(retireLine("r-0", 0, "raw zero"), retireLine("r-1", 1, "raw one")...)
	uids := rawUIDs(t, raw)
	writeRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", raw, 1)
	writeChParquet(t, filepath.Join(root, "events", "author=a", "machine=archive-ch", "harness=claude-code", "ch-export.parquet"), []chRow{
		archParityRow(uids[0], retireShared, "archive zero", 0, 5),
		archParityRow(uids[1], retireShared, "archive one", 1, 6),
		archParityRow("arch-only", retireShared, "archive extra", 7, 7),
		archParityRow("arch-second", retireOnly, "second session", 3, 8),
	})
	return uids
}

func retireLogs(t *testing.T, store string) (archive, raw []string) {
	t.Helper()
	ents, err := os.ReadDir(srcDir(store))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".light") && !strings.HasSuffix(e.Name(), ".heavy") {
			continue
		}
		if strings.Contains(e.Name(), archiveMemberSep) {
			archive = append(archive, e.Name())
		} else {
			raw = append(raw, e.Name())
		}
	}
	return archive, raw
}

func retireRows(t *testing.T, store string) []string {
	t.Helper()
	db, err := OpenDB(store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out []string
	for _, p := range db.parts {
		for d := 0; d < p.meta.Docs; d++ {
			r, err := p.row(d)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s|%s|%d|%s", r.UID, r.Session, r.Version, r.Text))
		}
	}
	slices.Sort(out)
	return out
}

func TestArchiveMembersKeepNoLogsAfterSettle(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	retireInputs(t, root)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	archive, raw := retireLogs(t, store)
	if len(archive) != 0 {
		t.Fatalf("archive members left %d log files after settle: %v", len(archive), archive)
	}
	if want := []string{"a__m__claude-code__s1.0.heavy", "a__m__claude-code__s1.0.light"}; !slices.Equal(raw, want) {
		t.Fatalf("raw logs %v, want %v", raw, want)
	}
}

func TestDroppedRawUIDReturnsTheArchiveCopy(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	uids := retireInputs(t, root)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if r := retireRows(t, store); !slices.Contains(r, fmt.Sprintf("%s|%s|0|raw zero", uids[0], retireShared)) {
		t.Fatalf("before the rewrite uid %s is not the raw copy: %v", uids[0], r)
	}

	rewritten := append(retireLine("r-9", 0, "raw nine"), retireLine("r-1", 1, "raw one")...)
	writeRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", rewritten, 2)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	got := retireRows(t, store)
	if want := fmt.Sprintf("%s|%s|0|archive zero", uids[0], retireShared); !slices.Contains(got, want) {
		t.Fatalf("after the raw source dropped uid %s the archive copy is missing: %v", uids[0], got)
	}

	cleanRoot, cleanStore := t.TempDir(), t.TempDir()
	retireInputs(t, cleanRoot)
	writeRawSource(t, cleanRoot, "a", "m", "claude-code", "s1", "/p/s1.jsonl", rewritten, 2)
	if _, err := Ingest(cleanRoot, cleanStore, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if want := retireRows(t, cleanStore); !slices.Equal(got, want) {
		t.Fatalf("rows differ from a clean build:\n got %v\nwant %v", got, want)
	}
}

func TestSettleRetiresOnceAndConverges(t *testing.T) {
	root := t.TempDir()
	retireInputs(t, root)
	once, twice := t.TempDir(), t.TempDir()
	if _, err := Ingest(root, once, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := Ingest(root, twice, time.Minute, true); err != nil {
			t.Fatal(err)
		}
	}
	if archive, _ := retireLogs(t, twice); len(archive) != 0 {
		t.Fatalf("archive members left logs after two cycles: %v", archive)
	}
	if hashTree(t, once) != hashTree(t, twice) {
		t.Fatal("a second cycle changed the store")
	}
}

func TestAppendAfterSettleDoesNotRematerializeArchiveMembers(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	uids := retireInputs(t, root)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if archive, _ := retireLogs(t, store); len(archive) != 0 {
		t.Fatalf("archive members kept logs after settle: %v", archive)
	}

	first := append(retireLine("r-0", 0, "raw zero"), retireLine("r-1", 1, "raw one")...)
	more := append(retireLine("r-2", 2, "raw two"), retireLine("r-3", 3, "raw three")...)
	appendRawSource(t, root, "a", "m", "claude-code", "s1", "/p/s1.jsonl", [][]byte{first, more}, time.Now().Add(time.Hour).UnixMilli())
	if _, err := Ingest(root, store, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if archive, _ := retireLogs(t, store); len(archive) != 0 {
		t.Fatalf("the append rematerialized archive members: %v", archive)
	}

	got := retireRows(t, store)
	for _, want := range []string{
		fmt.Sprintf("%s|%s|0|raw zero", uids[0], retireShared),
		fmt.Sprintf("%s|%s|2|raw two", rawUIDs(t, more)[0], retireShared),
		fmt.Sprintf("arch-only|%s|7|archive extra", retireShared),
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("row %q missing after the append: %v", want, got)
		}
	}

	cleanRoot, cleanStore := t.TempDir(), t.TempDir()
	retireInputs(t, cleanRoot)
	appendRawSource(t, cleanRoot, "a", "m", "claude-code", "s1", "/p/s1.jsonl", [][]byte{first, more}, 2)
	if _, err := Ingest(cleanRoot, cleanStore, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if want := retireRows(t, cleanStore); !slices.Equal(got, want) {
		t.Fatalf("rows differ from a clean build:\n got %v\nwant %v", got, want)
	}
}

// appendRawSource writes the raw source as one zstd frame per chunk, so a
// later chunk is an append whose frame starts at the earlier size.
func appendRawSource(t *testing.T, root, author, machine, harness, srcID, path string, chunks [][]byte, updatedMs int64) {
	t.Helper()
	enc, _ := zstd.NewWriter(nil)
	var zst, plain []byte
	for _, c := range chunks {
		zst = enc.EncodeAll(c, zst)
		plain = append(plain, c...)
	}
	writeRawManifest(t, root, author, machine, harness, srcID, path, int64(len(plain)), int64(len(zst)), updatedMs)
	file := filepath.Join(root, "raw", "author="+author, "machine="+machine, "harness="+harness, srcID+".jsonl.zst")
	if err := os.WriteFile(file, zst, 0o644); err != nil {
		t.Fatal(err)
	}
}

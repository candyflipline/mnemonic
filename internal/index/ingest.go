package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/normalize"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

type manifest struct {
	wire.Source
	ZstSize int64 `json:"zst_size"`
}

// parsed is where one source's parse stopped, independent of where its rows
// are kept: a checkpoint resumes from it and a cold part records it per member.
type parsed struct {
	Key        string          `json:"key"`
	Kind       Kind            `json:"kind"`
	Parser     int             `json:"parser"`
	Layout     int             `json:"layout"`
	Harness    string          `json:"harness"`
	SourceName string          `json:"source_name"`
	SourceFile string          `json:"source_file"`
	Path       string          `json:"path"`
	Author     string          `json:"author"`
	Machine    string          `json:"machine"`
	ZstSize    int64           `json:"zst_size"`
	WindowSHA  string          `json:"window_sha256"`
	State      normalize.State `json:"state"`
	Rows       int64           `json:"rows"`
	Sessions   []string        `json:"sessions"`
}

// ckpt is parsed plus where this disk keeps the rows. It is written by rename
// after both logs are fsynced, so the logs are valid up to LightLen and
// HeavyLen whatever happened after.
type ckpt struct {
	parsed
	Gen      int   `json:"gen"`
	LightLen int64 `json:"light_len"`
	HeavyLen int64 `json:"heavy_len"`
	ColdZst  int64 `json:"cold_zst"`

	ColdRows  int64  `json:"cold_rows,omitempty"`
	ColdLight int64  `json:"cold_light,omitempty"`
	ColdSHA   string `json:"cold_sha,omitempty"`
}

// logLayout is the shape of the rows a log holds. Layout 1 numbers a row's
// version by its place in its source, where the parser stamped the wall clock.
const logLayout = 1

// hot reports whether the source grew since it last settled into a cold part.
func (c ckpt) hot() bool { return c.ZstSize != c.ColdZst }

const window = 64 << 10

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

const (
	lightFrame = 128 << 10
	heavyFrame = 256 << 10
)

func srcDir(store string) string { return filepath.Join(store, "src") }

func ckptPath(store, key string) string { return filepath.Join(srcDir(store), key+".ckpt") }

func logPath(store, key string, gen int, kind string) string {
	return filepath.Join(srcDir(store), fmt.Sprintf("%s.%d.%s", key, gen, kind))
}

func sourceKey(k layout.Key) string {
	return k.Author + "__" + k.Machine + "__" + k.Harness + "__" + k.SrcID
}

type source struct {
	kind  Kind
	mpath string
	key   layout.Key
	src   manifest
}

func listSources(root string) ([]source, error) {
	rawDir := filepath.Join(root, "raw")
	paths, err := filepath.Glob(filepath.Join(rawDir, "author=*", "machine=*", "harness=*", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	mans := make([]manifest, len(paths))
	errs := make([]error, len(paths))
	bad := make([]error, len(paths))
	eachIndex(len(paths), func(i int) {
		b, err := os.ReadFile(paths[i])
		if err != nil {
			errs[i] = err
			return
		}
		bad[i] = json.Unmarshal(b, &mans[i])
	})
	var out []source
	for i, p := range paths {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if bad[i] != nil {
			fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", p, bad[i])
			continue
		}
		m := mans[i]
		rel, _ := filepath.Rel(rawDir, p)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 4 {
			continue
		}
		out = append(out, source{kind: KindStore, mpath: p, src: m, key: layout.Key{
			Author:  strings.TrimPrefix(parts[0], "author="),
			Machine: strings.TrimPrefix(parts[1], "machine="),
			Harness: strings.TrimPrefix(parts[2], "harness="),
			SrcID:   strings.TrimSuffix(parts[3], ".json"),
		}})
	}
	archive, err := listArchiveSources(root)
	if err != nil {
		return nil, err
	}
	return append(out, archive...), nil
}

func readCkpt(store, key string) (ckpt, bool) {
	var c ckpt
	b, err := os.ReadFile(ckptPath(store, key))
	if err != nil || json.Unmarshal(b, &c) != nil {
		return ckpt{}, false
	}
	return c, true
}

// readCkpts reads the checkpoint of each source, nil where it has none; an
// archive file's is left nil, its kind reads its own (archive.go).
func readCkpts(store string, sources []source) []*ckpt {
	out := make([]*ckpt, len(sources))
	eachIndex(len(sources), func(i int) {
		if sources[i].kind == KindArchive {
			return
		}
		if c, ok := readCkpt(store, sourceKey(sources[i].key)); ok {
			out[i] = &c
		}
	})
	return out
}

// eachIndex calls fn with 0..n-1 on GOMAXPROCS workers.
func eachIndex(n int, fn func(int)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(n, runtime.GOMAXPROCS(0)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				fn(i)
			}
		}()
	}
	wg.Wait()
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return renameSynced(tmp, path)
}

// renameSynced renames tmp to path with the parent directory fsynced before,
// so the entries of files written next to tmp are durable first, and after,
// so the rename itself survives power loss.
func renameSynced(tmp, path string) error {
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func windowSHA(raw *os.File, end int64) (string, error) {
	start := max(0, end-window)
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(raw, start, end-start)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openRaw(root string, s source) (*os.File, int64, error) {
	raw, err := os.Open(s.key.Raw(root))
	if err != nil {
		return nil, 0, err
	}
	fi, err := raw.Stat()
	if err != nil {
		raw.Close()
		return nil, 0, err
	}
	end := s.src.ZstSize
	if end <= 0 || end > fi.Size() {
		end = fi.Size()
	}
	return raw, end, nil
}

func wantCkpt(s source, parser normalize.Parser) ckpt {
	return ckpt{parsed: parsed{Key: sourceKey(s.key), Kind: s.kind, Parser: parser.Version(), Harness: s.src.Harness, SourceName: s.src.SourceName,
		SourceFile: s.src.Path, Author: s.key.Author, Machine: s.key.Machine, Layout: logLayout,
		Path: filepath.ToSlash(s.key.Events("")) /* lake path order */}}
}

// extends reports whether the input up to end still holds the bytes prev was
// parsed from, under the same kind, parser and identity, from a State that is
// not Provisional, so only the bytes past it parse. The byte test is the kind's.
func extends(raw *os.File, end int64, want, prev ckpt) bool {
	if prev.State.Provisional() || prev.Kind != want.Kind || prev.Parser != want.Parser || prev.Harness != want.Harness || prev.SourceName != want.SourceName ||
		prev.SourceFile != want.SourceFile || prev.Layout != want.Layout || prev.ZstSize > end {
		return false
	}
	return want.Kind.extends(raw, end, prev)
}

// storeExtends is KindStore's byte test: the 64 KB of raw before the
// checkpoint hash the same, and a zstd frame starts at it unless raw ends there.
func storeExtends(raw *os.File, end int64, prev ckpt) bool {
	if sum, err := windowSHA(raw, prev.ZstSize); err != nil || sum != prev.WindowSHA {
		return false
	}
	if prev.ZstSize < end {
		magic := make([]byte, 4)
		if _, err := raw.ReadAt(magic, prev.ZstSize); err != nil || !bytes.Equal(magic, zstdMagic) {
			return false
		}
	}
	return true
}

// ingestSource brings one source's logs up to its input, parsing only the
// bytes past its checkpoint when the input still extends it, and reports what
// it did and how many log bytes it appended. red (local mode) sits between the
// input and the parser: each line the parser reads has been through red.Line,
// exactly the bytes push uploads for the same line. cur is the source's
// checkpoint, nil for none, and the checkpoint the source has after is
// returned with the outcome (nil for an archive file, whose kind keeps its own).
func ingestSource(root, store string, s source, cur *ckpt, red *redact.Redactor) (Outcome, int64, *ckpt, error) {
	if s.kind == KindArchive {
		// Before normalize.For: archive harnesses do have parsers, but the
		// parquet is read by the archive kind, not parsed as raw (DESIGN.md,
		// Archive kind (d)).
		o, written, err := ingestArchive(store, s)
		return o, written, nil, err
	}
	key := sourceKey(s.key)
	parser, ok := normalize.For(s.src.Harness)
	if !ok {
		return Unchanged, 0, cur, nil
	}
	raw, end, err := s.kind.open(root, s)
	if err != nil {
		return 0, 0, cur, err
	}
	defer raw.Close()
	meta := normalize.SourceMeta{SourceName: s.src.SourceName, Harness: s.src.Harness, SourceFile: s.src.Path}
	want := wantCkpt(s, parser)
	var prev ckpt
	has := cur != nil
	if has {
		prev = *cur
	}
	resumed := has && extends(raw, end, want, prev)
	if resumed && prev.ZstSize == end {
		return Unchanged, 0, cur, nil
	}
	next := want
	var from ckpt
	if resumed {
		from = prev
		next.Gen, next.ColdZst = prev.Gen, prev.ColdZst
		next.ColdRows, next.ColdLight, next.ColdSHA = prev.ColdRows, prev.ColdLight, prev.ColdSHA
	} else if has {
		next.Gen = prev.Gen + 1
	}
	lightF, err := openLog(logPath(store, key, next.Gen, "light"), from.LightLen)
	if err != nil {
		return 0, 0, cur, err
	}
	defer lightF.Close()
	heavyF, err := openLog(logPath(store, key, next.Gen, "heavy"), from.HeavyLen)
	if err != nil {
		return 0, 0, cur, err
	}
	defer heavyF.Close()

	zr, err := s.kind.reader(raw, from.ZstSize, end)
	if err != nil {
		return 0, 0, cur, err
	}
	defer zr.Close()
	if red != nil {
		// Local mode: redact at ingest (redact.Line per line before parse,
		// exactly what push uploads), whichever kind carried the line in —
		// the harness files and the write tools' records alike. The
		// checkpoint stays in the input's own bytes, so resume still works
		// on the unredacted file.
		zr = newRedactReader(zr, red)
	}

	lw := &frameWriter{f: lightF, off: from.LightLen, target: lightFrame, enc: encoder}
	hw := &frameWriter{f: heavyF, off: from.HeavyLen, target: heavyFrame, enc: heavyEncoder}
	rows := from.Rows
	sessions := map[string]bool{}
	for _, s := range from.Sessions {
		sessions[s] = true
	}
	state, err := parser.Parse(zr, meta, from.State, func(ev *schema.Event) error {
		r := rowFrom(ev)
		r.Version = uint64(rows)
		sessions[r.Session] = true
		var heavy []byte
		heavy = appendStr(heavy, ev.PayloadJSON)
		heavy = appendStr(heavy, ev.TokenUsageJSON)
		fo, idx, err := hw.add(heavy)
		if err != nil {
			return err
		}
		r.HeavyFrame, r.HeavyIdx = uint64(fo), uint64(idx)
		if _, _, err := lw.add(r.append(nil)); err != nil {
			return err
		}
		rows++
		return nil
	})
	if err != nil {
		return 0, 0, cur, err
	}
	if err := hw.flush(); err != nil {
		return 0, 0, cur, err
	}
	if err := lw.flush(); err != nil {
		return 0, 0, cur, err
	}
	if err := lightF.Sync(); err != nil {
		return 0, 0, cur, err
	}
	if err := heavyF.Sync(); err != nil {
		return 0, 0, cur, err
	}
	sum, err := windowSHA(raw, end)
	if err != nil {
		return 0, 0, cur, err
	}
	next.ZstSize, next.WindowSHA, next.State = end, sum, state
	next.LightLen, next.HeavyLen, next.Rows = lw.off, hw.off, rows
	for s := range sessions {
		next.Sessions = append(next.Sessions, s)
	}
	sort.Strings(next.Sessions)
	b, _ := json.Marshal(next)
	if err := writeFileAtomic(ckptPath(store, key), b); err != nil {
		return 0, 0, cur, err
	}
	written := next.LightLen - from.LightLen + next.HeavyLen - from.HeavyLen
	switch {
	case resumed:
		return Resumed, written, &next, nil
	case has:
		return Reparsed, written, &next, nil
	}
	return Fresh, written, &next, nil
}

func appendStr(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func openLog(path string, validLen int64) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(validLen); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(validLen, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// frameWriter appends frames: u32 compressed length, u32 record count, then
// the zstd-compressed records, each a uvarint length and its bytes.
type frameWriter struct {
	f      *os.File
	target int
	enc    *zstd.Encoder
	off    int64
	buf    []byte
	n      int
}

var encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithLowerEncoderMem(true))
var heavyEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithLowerEncoderMem(true))

func (w *frameWriter) add(rec []byte) (int64, int, error) {
	if len(w.buf) > 0 && len(w.buf)+len(rec) > w.target {
		if err := w.flush(); err != nil {
			return 0, 0, err
		}
	}
	w.buf = binary.AppendUvarint(w.buf, uint64(len(rec)))
	w.buf = append(w.buf, rec...)
	w.n++
	return w.off, w.n - 1, nil
}

func (w *frameWriter) flush() error {
	if w.n == 0 {
		return nil
	}
	comp := w.enc.EncodeAll(w.buf, make([]byte, 8, 8+len(w.buf)/3))
	binary.LittleEndian.PutUint32(comp[0:4], uint32(len(comp)-8))
	binary.LittleEndian.PutUint32(comp[4:8], uint32(w.n))
	if _, err := w.f.Write(comp); err != nil {
		return err
	}
	w.off += int64(len(comp))
	w.buf, w.n = w.buf[:0], 0
	return nil
}

var decoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithDecoderLowmem(true))

// readFrame returns the records of the frame at off in r and the next offset.
func readFrame(r io.ReaderAt, off int64, scratch []byte) ([][]byte, int64, error) {
	var hdr [8]byte
	if _, err := r.ReadAt(hdr[:], off); err != nil {
		return nil, 0, err
	}
	clen := int64(binary.LittleEndian.Uint32(hdr[0:4]))
	n := int(binary.LittleEndian.Uint32(hdr[4:8]))
	comp := make([]byte, clen)
	if _, err := r.ReadAt(comp, off+8); err != nil {
		return nil, 0, err
	}
	plain, err := decoder.DecodeAll(comp, scratch[:0])
	if err != nil {
		return nil, 0, err
	}
	recs := make([][]byte, 0, n)
	b := plain
	for i := 0; i < n; i++ {
		l, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < l {
			return nil, 0, errShort
		}
		recs = append(recs, b[k:k+int(l)])
		b = b[k+int(l):]
	}
	return recs, off + 8 + clen, nil
}

// Outcome is what one Ingest did to one source.
type Outcome uint8

const (
	Unchanged Outcome = iota // the checkpoint matches raw: nothing parsed
	Resumed                  // the checkpoint extends: only the new bytes parsed
	Fresh                    // no checkpoint: parsed from byte 0, the first time on this store
	Reparsed                 // the checkpoint did not match the input or parser: parsed from byte 0 into gen+1
)

// IngestStats counts sources by Outcome, indexed by Outcome.
type IngestStats [4]int

// sweepBudget bounds the wall time one Ingest spends handing out version-stale
// sources, checked before each after the first, so a parser version bump
// re-parses over consecutive cycles instead of one. The caller starts the next
// cycle at once while any are left (server.Run); each source's own checkpoint
// is the lap's marker, so no sweep state is kept on disk.
const sweepBudget = 5 * time.Second

// Ingest brings store up to root's raw and reports what it did per source.
func Ingest(root, store string, quiet time.Duration, settleAll bool) (IngestStats, error) {
	sources, err := listSources(root)
	if err != nil {
		return IngestStats{}, err
	}
	return ingest(root, store, sources, quiet, settleAll, nil)
}

// IngestPlain brings store up to the machine's harness files the client globs
// match, indexed in place as KindPlain sources, plus the sources under the
// data dir around store — the records the write tools append through its
// store, and the raw copies old `mnemonic local` left there (the archive glob
// finds nothing). A copy whose key a live harness file carries is dropped: the
// plain file owns the key. One list and one cycle, so the manifest publish
// writes covers both and neither ingest drops the other's sources. store is
// the index dir under the data dir (layout.IndexDir), so root is its parent
// and the store sources' raw opens under it.
func IngestPlain(store string, globs []config.SourceGlob, author, machine string, opts IngestOptions) (IngestStats, error) {
	plain, err := listPlainSources(globs, author, machine)
	if err != nil {
		return IngestStats{}, err
	}
	root := filepath.Dir(store)
	tool, err := listSources(root)
	if err != nil {
		return IngestStats{}, err
	}
	// The live harness file wins: old `mnemonic local` pushed every harness
	// file into <root>/raw under the same key the plain source carries, so a
	// store source whose key a plain source holds is that raw copy, not a
	// second input — drop it, leaving the key one source and one checkpoint.
	// The copy stays on disk. A copy whose plain file no longer exists keeps
	// its key to itself and is ingested: it is the only copy left.
	plainKeys := make(map[string]bool, len(plain))
	for _, s := range plain {
		plainKeys[sourceKey(s.key)] = true
	}
	for _, s := range tool {
		if s.kind == KindStore && plainKeys[sourceKey(s.key)] {
			continue
		}
		plain = append(plain, s)
	}
	return ingest(root, store, plain, opts.Quiet, opts.SettleAll, opts.Red)
}

// ingest is the cycle both entry points share: parse the sources, publish the
// plan, and record the lap. root is where KindStore sources' raw lives; a
// KindPlain source opens its own path and ignores it. red is local mode's
// redactor: every line a parse reads goes through it, the bytes push's upload
// carries, so the index holds what the server path's raw holds.
//
// It is also the one place every writer passes through, so it holds the dir's
// writer lock for the lap: both package entries, Index.Ingest and
// Index.IngestPlain, and so serve's cycle, local's sink, export's pre-export
// ingest and the harness subcommand all exclude each other here, and a
// process holding the dir for its whole life (serve, local) borrows its own
// lock instead of refusing itself.
func ingest(root, store string, sources []source, quiet time.Duration, settleAll bool, red *redact.Redactor) (stats IngestStats, err error) {
	// One key, one source: two sources on one key would run two ingestSource
	// workers over the same checkpoint and light/heavy logs, each truncating
	// what the other appends. IngestPlain drops the raw copy behind a plain
	// file; every other caller's list is one source per key by construction,
	// so a duplicate here is refused loudly instead of corrupting the store.
	seen := make(map[string]bool, len(sources))
	for _, s := range sources {
		k := sourceKey(s.key)
		if seen[k] {
			return IngestStats{}, fmt.Errorf("ingest: duplicate source key %s", k)
		}
		seen[k] = true
	}
	release, err := IngestLock(store)
	if err != nil {
		return IngestStats{}, err
	}
	defer release()
	debug.SetGCPercent(25)
	var rec cycleRecord
	defer func() { recordCycle(store, rec) }()
	if err = os.MkdirAll(srcDir(store), 0o755); err != nil {
		return
	}
	take := takeCycle(store)
	cks := readCkpts(store, sources)
	outcomes, logBytes, stale, failSweep := parseSources(root, store, sources, cks, take.failSweep, red)
	for _, o := range outcomes {
		stats[o]++
	}
	rec = cycleRecord{stats: CycleStats{LogBytes: logBytes}, stale: stale,
		failed: len(sources) - len(outcomes), failSweep: failSweep}
	cs, deferred, perr := publish(store, sources, cks, quiet, settleAll, outcomes)
	cs.LogBytes = logBytes
	rec.stats, rec.settle = cs, deferred
	return stats, perr
}

// parseSources runs ingestSource over sources on GOMAXPROCS workers and
// returns each outcome by source key, how many log bytes the parses appended,
// how many version-stale sources the sweep budget left for the next cycle, and
// the failSweep to carry into it: the version-stale sources whose re-parse
// failed, so one poison source cannot keep the sweep cycling, plus skip for
// the keys already known to fail at their version. A source that failed has no
// outcome. cks holds each source's checkpoint (readCkpts) and is updated to
// the one each parse leaves.
func parseSources(root, store string, sources []source, cks []*ckpt, skip map[string]int, red *redact.Redactor) (outcomes map[string]Outcome, logBytes int64, stale int, failSweep map[string]int) {
	outcomes = make(map[string]Outcome, len(sources))
	failSweep = map[string]int{}
	for k, v := range skip {
		failSweep[k] = v
	}
	var now []int // pushed, fresh or appended: parsed whatever the sweep is doing
	var sweep []int
	for i, s := range sources {
		if versionStale(s, cks[i]) && failSweep[sourceKey(s.key)] != parserVersion(s) {
			sweep = append(sweep, i)
			continue
		}
		delete(failSweep, sourceKey(s.key))
		now = append(now, i)
	}
	var mu sync.Mutex
	work := make(chan int)
	done := make(chan []int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var failed []int
			for i := range work {
				s := sources[i]
				o, written, c, err := ingestSource(root, store, s, cks[i], red)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", s.mpath, err)
					failed = append(failed, i)
					continue
				}
				cks[i] = c
				mu.Lock()
				outcomes[sourceKey(s.key)] = o
				logBytes += written
				mu.Unlock()
			}
			done <- failed
		}()
	}
	for _, i := range now {
		work <- i
	}
	sweepEnd := time.Now().Add(sweepBudget)
	for n, i := range sweep {
		if n > 0 && time.Now().After(sweepEnd) {
			stale = len(sweep) - n
			break
		}
		work <- i
	}
	close(work)
	go func() {
		wg.Wait()
		close(done)
	}()
	for failed := range done {
		for _, i := range failed {
			if versionStale(sources[i], cks[i]) {
				failSweep[sourceKey(sources[i].key)] = parserVersion(sources[i])
			}
		}
	}
	return outcomes, logBytes, stale, failSweep
}

// parserVersion is the Version of s's parser, zero for a harness without one
// (such a source is never version-stale).
func parserVersion(s source) int {
	p, ok := normalize.For(s.src.Harness)
	if !ok {
		return 0
	}
	return p.Version()
}

// versionStale reports whether s would re-parse from byte 0 only because its
// parser has a newer Version than its checkpoint: no new bytes and none lost,
// so it is sweep work, bounded per cycle, not push work. KindPlain sources are
// never sweep work; their byte test is not the manifest's size. c is s's
// checkpoint, nil for none.
func versionStale(s source, c *ckpt) bool {
	if s.kind != KindStore || c == nil {
		return false
	}
	return c.Parser != parserVersion(s) && c.ZstSize == s.src.ZstSize
}

// publish plans the parts of the sources' checkpoints, builds the missing ones
// and points CURRENT at them, reporting what it wrote and how many components
// whose quiet window has passed it left hot for a later cycle (settleSet).
// cur holds each source's checkpoint (parseSources), outcomes what this
// cycle's parse did, by source key.
func publish(store string, sources []source, cur []*ckpt, quiet time.Duration, settleAll bool, outcomes map[string]Outcome) (CycleStats, int, error) {
	parsedAny := false
	for _, o := range outcomes {
		parsedAny = parsedAny || o != Unchanged
	}
	newest := int64(0)
	for _, s := range sources {
		newest = max(newest, s.src.UpdatedMs)
	}
	var cks []ckpt
	var due, touched []bool
	var srcs []stSource
	var keepExtra []string
	for si, s := range sources {
		if s.kind == KindArchive {
			// The archive file's checkpoint expands into its per-session
			// member checkpoints, which plan and build like any member; the
			// file itself never joins planning, and its sources row is
			// omitted — the cache's sources table skips archive files too,
			// so get_ingest_status and list_sessions must not count them.
			// Its rows still count in parts and ls rollups.
			fk := sourceKey(s.key)
			fc, ok := readCkpt(store, fk)
			if !ok {
				continue
			}
			keepExtra = append(keepExtra, ckptPath(store, fk))
			for _, sid := range fc.Sessions {
				mk := archiveMemberKey(fk, sid)
				c, ok := readCkpt(store, mk)
				if !ok {
					continue
				}
				cks = append(cks, c)
				due = append(due, settleAll || s.src.UpdatedMs <= newest-quiet.Milliseconds())
				touched = append(touched, outcomes[fk] > Unchanged)
			}
			continue
		}
		row := stSource{Author: s.key.Author, Machine: s.key.Machine, Harness: s.key.Harness, UpdatedMs: s.src.UpdatedMs, Size: s.src.Size}
		if c := cur[si]; c != nil {
			cks = append(cks, *c)
			due = append(due, settleAll || s.src.UpdatedMs <= newest-quiet.Milliseconds())
			touched = append(touched, outcomes[sourceKey(s.key)] > Unchanged)
			row.Events = c.Rows
		}
		srcs = append(srcs, row)
	}
	settle, deferred := settleSet(cks, due, touched, settleShards(settleAll, parsedAny))
	for i := range cks {
		if !settle[i] {
			continue
		}
		cks[i].ColdZst, cks[i].ColdRows, cks[i].ColdLight, cks[i].ColdSHA = cks[i].ZstSize, cks[i].Rows, cks[i].LightLen, cks[i].WindowSHA
		b, _ := json.Marshal(cks[i])
		if err := writeFileAtomic(ckptPath(store, cks[i].Key), b); err != nil {
			return CycleStats{}, deferred, err
		}
	}
	parts := planParts(cks)
	prev, _ := readManifest(store)
	byPrev := prevPartsMap(store, prev)
	back, err := rematerialize(store, cks, parts, byPrev)
	if err != nil {
		return CycleStats{}, deferred, err
	}
	if back > 0 {
		parts = planParts(cks)
	}
	ids := make([]string, 0, len(parts))
	var hot []string
	if err := os.MkdirAll(filepath.Join(store, "parts"), 0o755); err != nil {
		return CycleStats{}, deferred, err
	}
	keep := map[string]bool{}
	images := map[string]bool{}
	planned, err := buildParts(store, parts, prev, byPrev)
	if err != nil {
		return CycleStats{}, deferred, err
	}
	var cs CycleStats
	for _, p := range planned {
		if p.hot {
			hot = append(hot, p.id)
			if p.image != "" {
				keep[p.image], images[p.image] = true, true
			}
		}
		ids = append(ids, p.id)
		keep[p.id] = true
		if p.built {
			cs.PartsBuilt++
			cs.PartBytes += p.written
		}
	}
	cs.HotParts, cs.ColdParts = len(hot), len(ids)-len(hot)
	mb, _ := json.Marshal(manifestFile{Parts: ids, Hot: hot, Sources: srcs})
	if cur, err := os.ReadFile(filepath.Join(store, "CURRENT")); err != nil || !bytes.Equal(cur, mb) {
		if err := writeFileAtomic(filepath.Join(store, "CURRENT"), mb); err != nil {
			return cs, deferred, err
		}
		gcParts(store, keep)
		gcDeltas(store, images)
	}
	if err := retireSettled(store, cks, parts, planned); err != nil {
		return cs, deferred, err
	}
	return cs, deferred, gcLogs(store, cks, keepExtra)
}

type builtPart struct {
	id      string
	hot     bool
	image   string
	built   bool
	written int64
}

// buildParts builds every part of the plan that is missing on a pool of
// GOMAXPROCS workers. The parts are independent: each writes its own dir, and
// a delta only appends to the logs of the one image it extends. Results are in
// plan order, and the error is the first one in plan order.
func buildParts(store string, parts [][]ckpt, prev manifestFile, byPrev map[string]string) ([]builtPart, error) {
	out := make([]builtPart, len(parts))
	errs := make([]error, len(parts))
	var failedAt atomic.Int64
	failedAt.Store(math.MaxInt64)
	next := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if int64(i) > failedAt.Load() {
					continue
				}
				out[i], errs[i] = buildPart(store, parts[i], prev, byPrev)
				if errs[i] != nil {
					for cur := failedAt.Load(); int64(i) < cur && !failedAt.CompareAndSwap(cur, int64(i)); cur = failedAt.Load() {
					}
				}
			}
		}()
	}
	for i := range parts {
		next <- i
	}
	close(next)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func buildPart(store string, members []ckpt, prev manifestFile, byPrev map[string]string) (builtPart, error) {
	id := fingerprint(members)
	if slices.ContainsFunc(members, ckpt.hot) {
		if retiredJoinsBase(store, members, prev) {
			return buildHotImage(store, members, byPrev)
		}
		id, built, written, err := hotPart(store, members, prev, byPrev)
		if err != nil {
			return builtPart{}, err
		}
		p := builtPart{id: id, hot: true, built: built, written: written}
		if d, ok := readBase(store, id); ok && d.Image != id {
			p.image = d.Image
		}
		return p, nil
	}
	if _, err := os.Stat(partDir(store, id)); err != nil {
		if err := buildIndex(store, members, id, partDir(store, id), byPrev); err != nil {
			return builtPart{}, err
		}
		return builtPart{id: id, built: true, written: dirSize(partDir(store, id))}, nil
	}
	return builtPart{id: id}, nil
}

// retiredJoinsBase reports whether a delta append over the hot part members
// extend would take in a retired archive member that part does not hold: its
// rows are in no log to append, so the component is built whole instead.
func retiredJoinsBase(store string, members []ckpt, prev manifestFile) bool {
	if !slices.ContainsFunc(members, ckpt.retired) {
		return false
	}
	base, ok := deltaBase(store, members, prev)
	if !ok {
		return false
	}
	return slices.ContainsFunc(members, func(c ckpt) bool {
		return c.retired() && !slices.ContainsFunc(base.Members, func(m member) bool { return m.Key == c.Key })
	})
}

// buildHotImage builds the hot component members as a full part, hotPart
// without the delta path.
func buildHotImage(store string, members []ckpt, byPrev map[string]string) (builtPart, error) {
	id := fingerprint(members)
	for _, have := range []string{id, deltaID(members)} {
		if _, err := os.Stat(partDir(store, have)); err == nil {
			return builtPart{id: have, hot: true}, nil
		}
	}
	if err := buildIndex(store, members, id, partDir(store, id), byPrev); err != nil {
		return builtPart{}, err
	}
	return builtPart{id: id, hot: true, built: true, written: dirSize(partDir(store, id))}, nil
}

// dirSize is the bytes of the files directly in dir, what a part build or a
// delta commit wrote there.
func dirSize(dir string) int64 {
	var n int64
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// components returns, per item, a representative of its component: items that
// share a token are joined.
func components(tokens [][]string) []int {
	parent := make([]int, len(tokens))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	owner := map[string]int{}
	for i, ts := range tokens {
		for _, t := range ts {
			if j, ok := owner[t]; ok {
				parent[find(i)] = find(j)
			} else {
				owner[t] = i
			}
		}
	}
	root := make([]int, len(tokens))
	for i := range root {
		root[i] = find(i)
	}
	return root
}

// planParts groups sources that share a session into one component, so a
// session and every event version that can compete for its uids live in one
// part. A component with a hot source is a part of its own, rebuilt on each of
// its appends; the settled ones go to partShards cold parts by a hash of their
// least session id, rewritten only when a component settles into or leaves one.
func planParts(cks []ckpt) [][]ckpt {
	root, least, hot := componentsOf(cks)
	cold := map[uint32][]ckpt{}
	hotParts := map[string][]ckpt{}
	for i, c := range cks {
		r := root[i]
		if hot[r] {
			hotParts[least[r]] = append(hotParts[least[r]], c)
			continue
		}
		s := shardOf(least[r])
		cold[s] = append(cold[s], c)
	}
	keys := make([]uint32, 0, len(cold))
	for k := range cold {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([][]ckpt, 0, len(keys)+len(hotParts))
	for _, k := range keys {
		out = append(out, cold[k])
	}
	hk := make([]string, 0, len(hotParts))
	for k := range hotParts {
		hk = append(hk, k)
	}
	slices.Sort(hk)
	for _, k := range hk {
		out = append(out, hotParts[k])
	}
	return out
}

const partShards = 256

// componentsOf returns each checkpoint's component representative, and per
// representative the component's least session id (its key when it has no
// session) and whether any of its sources is hot.
func componentsOf(cks []ckpt) (root []int, least map[int]string, hot map[int]bool) {
	tokens := make([][]string, len(cks))
	for i, c := range cks {
		tokens[i] = c.Sessions
	}
	root = components(tokens)
	least, hot = map[int]string{}, map[int]bool{}
	for i, c := range cks {
		r := root[i]
		k := c.Key
		if len(c.Sessions) > 0 {
			k = c.Sessions[0]
		}
		if cur, ok := least[r]; !ok || k < cur {
			least[r] = k
		}
		hot[r] = hot[r] || c.hot()
	}
	return root, least, hot
}

func shardOf(least string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(least))
	return h.Sum32() % partShards
}

// settleShards is how many shards that already hold a settled component a
// cycle may rewrite to settle more components into them. Joining such a shard
// rewrites all of it, about 1/partShards of the corpus, so a cycle that
// parsed bytes (an append waiting to be searchable) rewrites none; a cycle
// that parsed nothing rewrites up to GOMAXPROCS, the width buildParts builds
// them at, and SettleAll every one.
func settleShards(settleAll, parsedAny bool) int {
	switch {
	case settleAll:
		return partShards
	case parsedAny:
		return 0
	}
	return runtime.GOMAXPROCS(0)
}

// settleSet picks the hot checkpoints that settle this cycle: every hot
// source of a component whose hot sources are all due, when the component's
// shard is one of the first budget shards by number that hold a settled
// component, or holds none yet and the cycle settles (budget > 0) or parsed
// the component (touched) or no shard is held at all: such a shard's part is
// the settling components alone, so a cold build, and one resumed after a
// crash mid-parse, settles in the cycle that publishes it. deferred counts the
// due components left hot for a later cycle.
func settleSet(cks []ckpt, due, touched []bool, budget int) (settle []bool, deferred int) {
	root, least, hot := componentsOf(cks)
	ready := map[int]bool{}
	for r, h := range hot {
		ready[r] = h
	}
	parsedComp := map[int]bool{}
	for i, c := range cks {
		if c.hot() && !due[i] {
			ready[root[i]] = false
		}
		parsedComp[root[i]] = parsedComp[root[i]] || touched[i]
	}
	held := map[uint32]bool{}
	for r, h := range hot {
		if !h {
			held[shardOf(least[r])] = true
		}
	}
	var busy []uint32
	for r, ok := range ready {
		if s := shardOf(least[r]); ok && held[s] && !slices.Contains(busy, s) {
			busy = append(busy, s)
		}
	}
	slices.Sort(busy)
	busy = busy[:min(budget, len(busy))]
	joins := func(r int) bool {
		s := shardOf(least[r])
		if held[s] {
			return ready[r] && slices.Contains(busy, s)
		}
		return ready[r] && (budget > 0 || parsedComp[r] || len(held) == 0)
	}
	for r, ok := range ready {
		if ok && !joins(r) {
			deferred++
		}
	}
	settle = make([]bool, len(cks))
	for i, c := range cks {
		settle[i] = c.hot() && joins(root[i])
	}
	return settle, deferred
}

// gcParts removes part dirs neither the manifest nor its deltas name, and any
// half-built part a killed build left behind.
func gcParts(store string, want map[string]bool) {
	ents, _ := os.ReadDir(filepath.Join(store, "parts"))
	for _, e := range ents {
		if !want[e.Name()] {
			os.RemoveAll(filepath.Join(store, "parts", e.Name()))
		}
	}
}

// gcLogs removes log generations no checkpoint names, plus any file in
// keepExtra: the archive files' checkpoints, which name member checkpoints
// rather than logs.
func gcLogs(store string, cks []ckpt, keepExtra []string) error {
	keep := map[string]bool{}
	for _, c := range cks {
		if !c.retired() {
			keep[filepath.Base(logPath(store, c.Key, c.Gen, "light"))] = true
			keep[filepath.Base(logPath(store, c.Key, c.Gen, "heavy"))] = true
		}
		keep[filepath.Base(ckptPath(store, c.Key))] = true
	}
	for _, p := range keepExtra {
		keep[filepath.Base(p)] = true
	}
	ents, err := os.ReadDir(srcDir(store))
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !keep[e.Name()] {
			os.Remove(filepath.Join(srcDir(store), e.Name()))
		}
	}
	return nil
}

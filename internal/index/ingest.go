package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

type manifest struct {
	wire.Source
	ZstSize int64 `json:"zst_size"`
}

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
	Offset     int64           `json:"zst_size"`
	WindowSHA  string          `json:"window_sha256"`
	State      normalize.State `json:"state"`
	Rows       int64           `json:"rows"`
	Sessions   []string        `json:"sessions"`
}

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

const logLayout = 1

type planEntry struct {
	ck           ckpt
	owner        *source
	due, touched bool
}

func writeCkpt(store string, c ckpt) error {
	b, _ := json.Marshal(c)
	return writeFileAtomic(ckptPath(store, c.Key), b)
}

func (c ckpt) hot() bool { return c.Offset != c.ColdZst }

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
	ck    *ckpt
}

func storeSource(root string, m store.Manifest) source {
	return source{kind: KindStore, mpath: m.Key.Manifest(root), key: m.Key, src: manifest{Source: m.Source, ZstSize: m.ZstSize}}
}

type sourceSet struct {
	list []source
	keys map[string]bool
}

func (ss *sourceSet) insert(i int, s source) error {
	k := sourceKey(s.key)
	if ss.keys[k] {
		return fmt.Errorf("ingest: duplicate source key %s", k)
	}
	if ss.keys == nil {
		ss.keys = map[string]bool{}
	}
	ss.keys[k] = true
	ss.list = slices.Insert(ss.list, i, s)
	return nil
}

func (ss *sourceSet) add(s ...source) error {
	for _, s := range s {
		if err := ss.insert(len(ss.list), s); err != nil {
			return err
		}
	}
	return nil
}

func (ss *sourceSet) remove(i int) {
	delete(ss.keys, sourceKey(ss.list[i].key))
	ss.list = slices.Delete(ss.list, i, i+1)
}

func listSources(root string) (*sourceSet, error) {
	rawDir := filepath.Join(root, "raw")
	paths, err := filepath.Glob(filepath.Join(rawDir, "author=*", "machine=*", "harness=*", "*.json"))
	if err != nil {
		return nil, err
	}
	mans := make([]store.Manifest, len(paths))
	ok := make([]bool, len(paths))
	errs := make([]error, len(paths))
	eachIndex(len(paths), func(i int) {
		var m manifest
		m, ok[i], errs[i] = readSourceManifest(paths[i])
		rel, _ := filepath.Rel(rawDir, paths[i])
		parts := strings.Split(rel, string(filepath.Separator))
		mans[i] = store.Manifest{Source: m.Source, ZstSize: m.ZstSize, Key: layout.Key{
			Author:  strings.TrimPrefix(parts[0], "author="),
			Machine: strings.TrimPrefix(parts[1], "machine="),
			Harness: strings.TrimPrefix(parts[2], "harness="),
			SrcID:   strings.TrimSuffix(parts[3], ".json"),
		}}
	})
	var listed []store.Manifest
	for i := range paths {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if ok[i] {
			listed = append(listed, mans[i])
		}
	}
	return sourcesOf(root, listed)
}

func sourcesOf(root string, manifests []store.Manifest) (*sourceSet, error) {
	stored := make([]source, len(manifests))
	for i, m := range manifests {
		stored[i] = storeSource(root, m)
	}
	slices.SortFunc(stored, func(a, b source) int { return strings.Compare(a.mpath, b.mpath) })
	archive, err := listArchiveSources(root)
	if err != nil {
		return nil, err
	}
	ss := &sourceSet{}
	if err := ss.add(stored...); err != nil {
		return nil, err
	}
	return ss, ss.add(archive...)
}

func readCkpt(store, key string) (ckpt, bool) {
	var c ckpt
	b, err := os.ReadFile(ckptPath(store, key))
	if err != nil || json.Unmarshal(b, &c) != nil {
		return ckpt{}, false
	}
	return c, true
}

func readCkpts(store string, sources []source) {
	eachIndex(len(sources), func(i int) {
		if sources[i].kind == KindArchive {
			return
		}
		if c, ok := readCkpt(store, sourceKey(sources[i].key)); ok {
			sources[i].ck = &c
		}
	})
}

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
		Path: filepath.ToSlash(s.key.Events(""))}}
}

func extends(raw *os.File, end int64, want, prev ckpt) bool {
	if !prev.sameInput(want) || prev.State.Provisional() && prev.Offset < end {
		return false
	}
	return want.Kind.extends(raw, end, prev)
}

func (c ckpt) sameInput(want ckpt) bool {
	return c.Kind == want.Kind && c.Parser == want.Parser && c.Harness == want.Harness && c.SourceName == want.SourceName &&
		c.SourceFile == want.SourceFile && c.Layout == want.Layout
}

func (s *source) ingest(root, store string, red *redact.Redactor) (o Outcome, logBytes int64, err error) {
	if s.kind == KindArchive {
		return ingestArchive(store, *s)
	}
	cur := s.ck
	key := sourceKey(s.key)
	parser, ok := normalize.For(s.src.Harness)
	if !ok {
		return Unchanged, 0, nil
	}
	raw, end, err := s.kind.open(root, *s)
	if err != nil {
		return 0, 0, err
	}
	defer raw.Close()
	meta := normalize.SourceMeta{SourceName: s.src.SourceName, Harness: s.src.Harness, SourceFile: s.src.Path}
	want := wantCkpt(*s, parser)
	var prev ckpt
	has := cur != nil
	if has {
		prev = *cur
	}
	resumed := has && extends(raw, end, want, prev)
	if resumed && prev.Offset == end {
		return Unchanged, 0, nil
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
	logs, err := openFrameLogs(store, key, next.Gen, from.LightLen, from.HeavyLen)
	if err != nil {
		return 0, 0, err
	}
	defer logs.close()

	zr, err := s.kind.reader(raw, from.Offset, end)
	if err != nil {
		return 0, 0, err
	}
	if red != nil {
		zr = newRedactReader(zr, red)
	}
	defer zr.Close()

	rows := from.Rows
	sessions := map[string]bool{}
	for _, s := range from.Sessions {
		sessions[s] = true
	}
	state, err := parser.Parse(zr, meta, from.State, func(ev *schema.Event) error {
		r := rowFrom(ev)
		r.Version = uint64(rows)
		sessions[r.Session] = true
		if err := logs.add(&r, ev.PayloadJSON, ev.TokenUsageJSON); err != nil {
			return err
		}
		rows++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if next.LightLen, next.HeavyLen, err = logs.sync(); err != nil {
		return 0, 0, err
	}
	sum, err := windowSHA(raw, end)
	if err != nil {
		return 0, 0, err
	}
	next.Offset, next.WindowSHA, next.State, next.Rows = end, sum, state, rows
	for s := range sessions {
		next.Sessions = append(next.Sessions, s)
	}
	sort.Strings(next.Sessions)
	if err := writeCkpt(store, next); err != nil {
		return 0, 0, err
	}
	s.ck = &next
	logBytes = next.LightLen - from.LightLen + next.HeavyLen - from.HeavyLen
	switch {
	case resumed:
		return Resumed, logBytes, nil
	case has:
		return Reparsed, logBytes, nil
	}
	return Fresh, logBytes, nil
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

type frameLogs struct{ light, heavy *frameWriter }

func openFrameLogs(store, key string, gen int, lightLen, heavyLen int64) (frameLogs, error) {
	lf, err := openLog(logPath(store, key, gen, "light"), lightLen)
	if err != nil {
		return frameLogs{}, err
	}
	hf, err := openLog(logPath(store, key, gen, "heavy"), heavyLen)
	if err != nil {
		lf.Close()
		return frameLogs{}, err
	}
	return frameLogs{
		light: &frameWriter{f: lf, off: lightLen, target: lightFrame, enc: encoder},
		heavy: &frameWriter{f: hf, off: heavyLen, target: heavyFrame, enc: heavyEncoder},
	}, nil
}

func (l frameLogs) add(r *Row, payload, tokenUsage string) error {
	fo, idx, err := l.heavy.add(appendStr(appendStr(nil, payload), tokenUsage))
	if err != nil {
		return err
	}
	r.HeavyFrame, r.HeavyIdx = uint64(fo), uint64(idx)
	_, _, err = l.light.add(r.append(nil))
	return err
}

func (l frameLogs) sync() (lightLen, heavyLen int64, err error) {
	for _, w := range []*frameWriter{l.heavy, l.light} {
		if err := w.flush(); err != nil {
			return 0, 0, err
		}
	}
	for _, w := range []*frameWriter{l.light, l.heavy} {
		if err := w.f.Sync(); err != nil {
			return 0, 0, err
		}
	}
	return l.light.off, l.heavy.off, nil
}

func (l frameLogs) close() error { return errors.Join(l.heavy.f.Close(), l.light.f.Close()) }

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

type Outcome uint8

const (
	Unchanged Outcome = iota
	Resumed
	Fresh
	Reparsed
)

type IngestStats [4]int

const sweepBudget = 5 * time.Second

type writerLock struct{ store string }

func withWriterLock(store string, fn func(writerLock) error) error {
	release, err := IngestLock(store)
	if err != nil {
		return err
	}
	defer release()
	return fn(writerLock{store: store})
}

func Ingest(root, store string, quiet time.Duration, settleAll bool) (IngestStats, error) {
	return ingest(root, store, func() (*sourceSet, error) { return listSources(root) }, quiet, settleAll, nil)
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
	root := filepath.Dir(store)
	return ingest(root, store, func() (*sourceSet, error) { return listPlainAndStore(root, globs, author, machine) }, opts.Quiet, opts.SettleAll, opts.Red)
}

func listPlainAndStore(root string, globs []config.SourceGlob, author, machine string) (*sourceSet, error) {
	plain, err := listPlainSources(globs, author, machine)
	if err != nil {
		return nil, err
	}
	tool, err := listSources(root)
	if err != nil {
		return nil, err
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
	sources := &sourceSet{}
	if err := sources.add(plain...); err != nil {
		return nil, err
	}
	for _, s := range tool.list {
		if s.kind == KindStore && plainKeys[sourceKey(s.key)] {
			continue
		}
		if err := sources.add(s); err != nil {
			return nil, err
		}
	}
	return sources, nil
}

// ingest is the cycle both entry points share: parse the sources, publish the
// plan, and record the lap. root is where KindStore sources' raw lives; a
// KindPlain source opens its own path and ignores it. red is local mode's
// redactor: every line a parse reads goes through it, the bytes push's upload
// carries, so the index holds what the server path's raw holds.
func ingest(root, store string, list func() (*sourceSet, error), quiet time.Duration, settleAll bool, red *redact.Redactor) (stats IngestStats, err error) {
	err = withWriterLock(store, func(w writerLock) error {
		sources, err := list()
		if err != nil {
			return err
		}
		readCkpts(store, sources.list)
		var opened map[string]*Part
		stats, _, opened, err = sources.cycle(w, root, allOf(sources.list), quiet, settleAll, red)
		closeParts(opened)
		return err
	})
	return stats, err
}

func allOf(sources []source) []int {
	all := make([]int, len(sources))
	for i := range all {
		all[i] = i
	}
	return all
}

// cycle parses the sources work indexes, counts every other one Unchanged,
// publishes them all and records the lap; each source's ck becomes the one
// its parse or settle leaves.
func (ss *sourceSet) cycle(w writerLock, root string, work []int, quiet time.Duration, settleAll bool, red *redact.Redactor) (stats IngestStats, outcomes map[string]Outcome, opened map[string]*Part, err error) {
	store, sources := w.store, ss.list
	debug.SetGCPercent(25)
	var rec cycleRecord
	defer func() { recordCycle(store, rec) }()
	if err = os.MkdirAll(srcDir(store), 0o755); err != nil {
		return
	}
	take := takeCycle(store)
	outcomes, logBytes, stale, failSweep := parseSources(root, store, sources, work, take.failSweep, red)
	for _, o := range outcomes {
		stats[o]++
	}
	stats[Unchanged] += len(sources) - len(work)
	rec = cycleRecord{stats: CycleStats{LogBytes: logBytes}, stale: stale,
		failed: len(work) - len(outcomes), failSweep: failSweep}
	cs, deferred, opened, perr := publish(store, sources, quiet, settleAll, outcomes)
	cs.LogBytes = logBytes
	rec.stats, rec.settle = cs, deferred
	return stats, outcomes, opened, perr
}

// parseSources runs ingest over the sources work indexes on GOMAXPROCS workers and
// returns each outcome by source key, how many log bytes the parses appended,
// how many version-stale sources the sweep budget left for the next cycle, and
// the failSweep to carry into it: the version-stale sources whose re-parse
// failed, so one poison source cannot keep the sweep cycling, plus skip for
// the keys already known to fail at their version. A source that failed has no
// outcome. Each source's ck (readCkpts) is updated to the one its parse
// leaves.
func parseSources(root, store string, sources []source, work []int, skip map[string]int, red *redact.Redactor) (outcomes map[string]Outcome, logBytes int64, stale int, failSweep map[string]int) {
	outcomes = make(map[string]Outcome, len(work))
	failSweep = map[string]int{}
	for k, v := range skip {
		failSweep[k] = v
	}
	var now []int
	var sweep []int
	for _, i := range work {
		s := sources[i]
		if versionStale(s) && failSweep[sourceKey(s.key)] != parserVersion(s) {
			sweep = append(sweep, i)
			continue
		}
		delete(failSweep, sourceKey(s.key))
		now = append(now, i)
	}
	var mu sync.Mutex
	jobs := make(chan int)
	done := make(chan []int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var failed []int
			for i := range jobs {
				s := &sources[i]
				o, written, err := s.ingest(root, store, red)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", s.mpath, err)
					failed = append(failed, i)
					continue
				}
				mu.Lock()
				outcomes[sourceKey(s.key)] = o
				logBytes += written
				mu.Unlock()
			}
			done <- failed
		}()
	}
	for _, i := range now {
		jobs <- i
	}
	sweepEnd := time.Now().Add(sweepBudget)
	for n, i := range sweep {
		if n > 0 && time.Now().After(sweepEnd) {
			stale = len(sweep) - n
			break
		}
		jobs <- i
	}
	close(jobs)
	go func() {
		wg.Wait()
		close(done)
	}()
	for failed := range done {
		for _, i := range failed {
			if versionStale(sources[i]) {
				failSweep[sourceKey(sources[i].key)] = parserVersion(sources[i])
			}
		}
	}
	return outcomes, logBytes, stale, failSweep
}

func parserVersion(s source) int {
	p, ok := normalize.For(s.src.Harness)
	if !ok {
		return 0
	}
	return p.Version()
}

func versionStale(s source) bool {
	if !s.kind.sweepable() || s.ck == nil {
		return false
	}
	return s.ck.Parser != parserVersion(s) && s.ck.Offset == s.src.ZstSize
}

// publish plans the parts of the sources' checkpoints, builds the missing ones
// and points CURRENT at them, reporting what it wrote and how many components
// whose quiet window has passed it left hot for a later cycle (settleSet).
// Each source's ck (parseSources) takes the one a settle writes; outcomes is
// what this cycle's parse did, by source key. The delta parts its appends
// opened come back by id, owned by the caller.
func publish(store string, sources []source, quiet time.Duration, settleAll bool, outcomes map[string]Outcome) (CycleStats, int, map[string]*Part, error) {
	parsedAny := false
	for _, o := range outcomes {
		parsedAny = parsedAny || o != Unchanged
	}
	newest := int64(0)
	for _, s := range sources {
		newest = max(newest, s.src.UpdatedMs)
	}
	var entries []planEntry
	var srcs []stSource
	var keepExtra []string
	for i := range sources {
		s := &sources[i]
		due := settleAll || s.src.UpdatedMs <= newest-quiet.Milliseconds()
		touched := outcomes[sourceKey(s.key)] > Unchanged
		if s.kind == KindArchive {
			// No sources row: the cache's sources table skips archive files,
			// so get_ingest_status and list_sessions do not count them.
			members, ok := readArchiveMembers(store, sourceKey(s.key))
			if ok {
				keepExtra = append(keepExtra, ckptPath(store, sourceKey(s.key)))
			}
			for _, c := range members {
				entries = append(entries, planEntry{ck: c, due: due, touched: touched})
			}
			continue
		}
		row := stSource{Author: s.key.Author, Machine: s.key.Machine, Harness: s.key.Harness, UpdatedMs: s.src.UpdatedMs, Size: s.src.Size}
		if s.ck != nil {
			entries = append(entries, planEntry{ck: *s.ck, owner: s, due: due, touched: touched})
			row.Events = s.ck.Rows
		}
		srcs = append(srcs, row)
	}
	settle, deferred := settleSet(entries, settleShards(settleAll, parsedAny))
	for i := range entries {
		if !settle[i] {
			continue
		}
		e := &entries[i]
		e.ck.ColdZst, e.ck.ColdRows, e.ck.ColdLight, e.ck.ColdSHA = e.ck.Offset, e.ck.Rows, e.ck.LightLen, e.ck.WindowSHA
		if err := writeCkpt(store, e.ck); err != nil {
			return CycleStats{}, deferred, nil, err
		}
		if e.owner != nil {
			c := e.ck
			e.owner.ck = &c
		}
	}
	cks := make([]ckpt, len(entries))
	for i, e := range entries {
		cks[i] = e.ck
	}
	parts := planParts(cks)
	prev, _ := readManifest(store)
	byPrev := prevPartsMap(store, prev)
	back, err := rematerialize(store, cks, parts, byPrev)
	if err != nil {
		return CycleStats{}, deferred, nil, err
	}
	if back > 0 {
		parts = planParts(cks)
	}
	ids := make([]string, 0, len(parts))
	var hot []string
	if err := os.MkdirAll(filepath.Join(store, "parts"), 0o755); err != nil {
		return CycleStats{}, deferred, nil, err
	}
	keep := map[string]bool{}
	images := map[string]bool{}
	planned, err := buildParts(store, parts, prev, byPrev)
	if err != nil {
		return CycleStats{}, deferred, nil, err
	}
	opened := map[string]*Part{}
	var cs CycleStats
	for _, p := range planned {
		if p.opened != nil {
			opened[p.id] = p.opened
		}
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
			return cs, deferred, opened, err
		}
		gcParts(store, keep)
		gcDeltas(store, images)
	}
	if err := retireSettled(store, cks, parts, planned); err != nil {
		return cs, deferred, opened, err
	}
	return cs, deferred, opened, gcLogs(store, cks, keepExtra)
}

type builtPart struct {
	id      string
	hot     bool
	image   string
	built   bool
	written int64
	opened  *Part
}

func closeParts(parts map[string]*Part) {
	for _, p := range parts {
		p.Close()
	}
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
			for _, p := range out {
				if p.opened != nil {
					p.opened.Close()
				}
			}
			return nil, err
		}
	}
	return out, nil
}

func buildPart(store string, members []ckpt, prev manifestFile, byPrev map[string]string) (builtPart, error) {
	id := fingerprint(members)
	if annotationOnly(members) {
		if _, err := os.Stat(partDir(store, id)); err == nil {
			return builtPart{id: id, hot: slices.ContainsFunc(members, ckpt.hot)}, nil
		}
		if err := buildIndex(store, members, id, partDir(store, id), byPrev); err != nil {
			return builtPart{}, err
		}
		return builtPart{id: id, hot: slices.ContainsFunc(members, ckpt.hot), built: true, written: dirSize(partDir(store, id))}, nil
	}
	if slices.ContainsFunc(members, ckpt.hot) {
		if retiredJoinsBase(store, members, prev) {
			return buildHotImage(store, members, byPrev)
		}
		id, built, written, opened, err := hotPart(store, members, prev, byPrev)
		if err != nil {
			return builtPart{}, err
		}
		p := builtPart{id: id, hot: true, built: built, written: written, opened: opened}
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
// session) and whether any of its sources is hot. An annotation checkpoint
// contributes no session tokens to the join: it is a component of its own,
// keyed by its source key, whose part holds the annotations alone and joins
// its targets at read time.
func componentsOf(cks []ckpt) (root []int, least map[int]string, hot map[int]bool) {
	tokens := make([][]string, len(cks))
	for i, c := range cks {
		if c.Harness != schema.AnnotationHarness {
			tokens[i] = c.Sessions
		}
	}
	root = components(tokens)
	least, hot = map[int]string{}, map[int]bool{}
	for i, c := range cks {
		r := root[i]
		k := c.Key
		if c.Harness != schema.AnnotationHarness && len(c.Sessions) > 0 {
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

func annotationOnly(members []ckpt) bool {
	return len(members) > 0 && !slices.ContainsFunc(members, func(c ckpt) bool { return c.Harness != schema.AnnotationHarness })
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
func settleSet(entries []planEntry, budget int) (settle []bool, deferred int) {
	cks := make([]ckpt, len(entries))
	for i, e := range entries {
		cks[i] = e.ck
	}
	root, least, hot := componentsOf(cks)
	ready := map[int]bool{}
	for r, h := range hot {
		ready[r] = h
	}
	parsedComp := map[int]bool{}
	for i, e := range entries {
		if e.ck.hot() && !e.due {
			ready[root[i]] = false
		}
		parsedComp[root[i]] = parsedComp[root[i]] || e.touched
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

func gcParts(store string, want map[string]bool) {
	ents, _ := os.ReadDir(filepath.Join(store, "parts"))
	for _, e := range ents {
		if !want[e.Name()] {
			os.RemoveAll(filepath.Join(store, "parts", e.Name()))
		}
	}
}

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

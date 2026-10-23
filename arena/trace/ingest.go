package trace

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

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/normalize"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

type manifest struct {
	wire.Source
	ZstSize int64 `json:"zst_size"`
}

// ckpt is where one source's parse stopped. It is written by rename after
// both logs are fsynced, so the logs are valid up to LightLen and HeavyLen
// whatever happened after.
type ckpt struct {
	Key        string          `json:"key"`
	Parser     int             `json:"parser"`
	Harness    string          `json:"harness"`
	SourceName string          `json:"source_name"`
	SourceFile string          `json:"source_file"`
	ZstSize    int64           `json:"zst_size"`
	WindowSHA  string          `json:"window_sha256"`
	State      normalize.State `json:"state"`
	Gen        int             `json:"gen"`
	LightLen   int64           `json:"light_len"`
	HeavyLen   int64           `json:"heavy_len"`
	Rows       int64           `json:"rows"`
	Path       string          `json:"path"`
	Author     string          `json:"author"`
	Machine    string          `json:"machine"`
	Sessions   []string        `json:"sessions"`
	ColdZst    int64           `json:"cold_zst"`
	Layout     int             `json:"layout"`
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
	var out []source
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var m manifest
		if err := json.Unmarshal(b, &m); err != nil {
			fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", p, err)
			continue
		}
		rel, _ := filepath.Rel(rawDir, p)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 4 {
			continue
		}
		out = append(out, source{mpath: p, src: m, key: layout.Key{
			Author:  strings.TrimPrefix(parts[0], "author="),
			Machine: strings.TrimPrefix(parts[1], "machine="),
			Harness: strings.TrimPrefix(parts[2], "harness="),
			SrcID:   strings.TrimSuffix(parts[3], ".json"),
		}})
	}
	return out, nil
}

func readCkpt(store, key string) (ckpt, bool) {
	var c ckpt
	b, err := os.ReadFile(ckptPath(store, key))
	if err != nil || json.Unmarshal(b, &c) != nil {
		return ckpt{}, false
	}
	return c, true
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

// ingestSource brings one source's logs up to its raw, parsing only the bytes
// past its checkpoint when the raw still extends it. It reports whether the
// checkpoint moved.
func ingestSource(root, store string, s source) (bool, error) {
	key := sourceKey(s.key)
	parser, ok := normalize.For(s.src.Harness)
	if !ok {
		return false, nil
	}
	raw, err := os.Open(s.key.Raw(root))
	if err != nil {
		return false, err
	}
	defer raw.Close()
	fi, err := raw.Stat()
	if err != nil {
		return false, err
	}
	end := s.src.ZstSize
	if end <= 0 || end > fi.Size() {
		end = fi.Size()
	}
	want := ckpt{Key: key, Parser: normalize.Version, Harness: s.src.Harness, SourceName: s.src.SourceName,
		SourceFile: s.src.Path, Author: s.key.Author, Machine: s.key.Machine, Layout: logLayout,
		Path: filepath.ToSlash(s.key.Events("")) /* lake path order */}
	prev, has := readCkpt(store, key)
	resumed := false
	if has && prev.Parser == want.Parser && prev.Harness == want.Harness && prev.SourceName == want.SourceName &&
		prev.SourceFile == want.SourceFile && prev.Layout == want.Layout && prev.ZstSize <= end {
		if sum, err := windowSHA(raw, prev.ZstSize); err == nil && sum == prev.WindowSHA {
			resumed = true
			if prev.ZstSize < end {
				magic := make([]byte, 4)
				if _, err := raw.ReadAt(magic, prev.ZstSize); err != nil || !bytes.Equal(magic, zstdMagic) {
					resumed = false
				}
			}
		}
	}
	if resumed && prev.ZstSize == end {
		return false, nil
	}
	next := want
	var from ckpt
	if resumed {
		from = prev
		next.Gen, next.ColdZst = prev.Gen, prev.ColdZst
	} else if has {
		next.Gen = prev.Gen + 1
	}
	lightF, err := openLog(logPath(store, key, next.Gen, "light"), from.LightLen)
	if err != nil {
		return false, err
	}
	defer lightF.Close()
	heavyF, err := openLog(logPath(store, key, next.Gen, "heavy"), from.HeavyLen)
	if err != nil {
		return false, err
	}
	defer heavyF.Close()

	zr, err := zstd.NewReader(io.NewSectionReader(raw, from.ZstSize, end-from.ZstSize), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return false, err
	}
	defer zr.Close()

	lw := &frameWriter{f: lightF, off: from.LightLen, target: lightFrame, enc: encoder}
	hw := &frameWriter{f: heavyF, off: from.HeavyLen, target: heavyFrame, enc: heavyEncoder}
	rows := from.Rows
	sessions := map[string]bool{}
	for _, s := range from.Sessions {
		sessions[s] = true
	}
	state, err := parser.Parse(zr, normalize.SourceMeta{SourceName: s.src.SourceName, Harness: s.src.Harness, SourceFile: s.src.Path},
		from.State, func(ev *schema.Event) error {
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
		return false, err
	}
	if err := hw.flush(); err != nil {
		return false, err
	}
	if err := lw.flush(); err != nil {
		return false, err
	}
	if err := lightF.Sync(); err != nil {
		return false, err
	}
	if err := heavyF.Sync(); err != nil {
		return false, err
	}
	sum, err := windowSHA(raw, end)
	if err != nil {
		return false, err
	}
	next.ZstSize, next.WindowSHA, next.State = end, sum, state
	next.LightLen, next.HeavyLen, next.Rows = lw.off, hw.off, rows
	for s := range sessions {
		next.Sessions = append(next.Sessions, s)
	}
	sort.Strings(next.Sessions)
	b, _ := json.Marshal(next)
	if err := writeFileAtomic(ckptPath(store, key), b); err != nil {
		return false, err
	}
	return true, nil
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

func Ingest(root, store string, quiet time.Duration, settleAll bool) error {
	debug.SetGCPercent(25)
	if err := os.MkdirAll(srcDir(store), 0o755); err != nil {
		return err
	}
	sources, err := listSources(root)
	if err != nil {
		return err
	}
	work := make(chan source)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				if _, err := ingestSource(root, store, s); err != nil {
					fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", s.mpath, err)
				}
			}
		}()
	}
	for _, s := range sources {
		work <- s
	}
	close(work)
	wg.Wait()
	newest := int64(0)
	for _, s := range sources {
		newest = max(newest, s.src.UpdatedMs)
	}
	var cks []ckpt
	var srcs []stSource
	for _, s := range sources {
		row := stSource{Author: s.key.Author, Machine: s.key.Machine, UpdatedMs: s.src.UpdatedMs, Size: s.src.Size}
		if c, ok := readCkpt(store, sourceKey(s.key)); ok {
			if c.hot() && (settleAll || s.src.UpdatedMs <= newest-quiet.Milliseconds()) {
				c.ColdZst = c.ZstSize
				b, _ := json.Marshal(c)
				if err := writeFileAtomic(ckptPath(store, c.Key), b); err != nil {
					return err
				}
			}
			cks = append(cks, c)
			row.Events = c.Rows
		}
		srcs = append(srcs, row)
	}
	parts := planParts(cks)
	ids := make([]string, 0, len(parts))
	var hot []string
	if err := os.MkdirAll(filepath.Join(store, "parts"), 0o755); err != nil {
		return err
	}
	prev, _ := readManifest(store)
	keep := map[string]bool{}
	images := map[string]bool{}
	planned, err := buildParts(store, parts, prev)
	if err != nil {
		return err
	}
	for _, p := range planned {
		if p.hot {
			hot = append(hot, p.id)
			if p.image != "" {
				keep[p.image], images[p.image] = true, true
			}
		}
		ids = append(ids, p.id)
		keep[p.id] = true
	}
	mb, _ := json.Marshal(manifestFile{Parts: ids, Hot: hot, Sources: srcs})
	if cur, err := os.ReadFile(filepath.Join(store, "CURRENT")); err == nil && bytes.Equal(cur, mb) {
		return nil
	}
	if err := writeFileAtomic(filepath.Join(store, "CURRENT"), mb); err != nil {
		return err
	}
	gcParts(store, keep)
	gcDeltas(store, images)
	return gcLogs(store, cks)
}

type builtPart struct {
	id    string
	hot   bool
	image string
}

// buildParts builds every part of the plan that is missing on a pool of
// GOMAXPROCS workers. The parts are independent: each writes its own dir, and
// a delta only appends to the logs of the one image it extends. Results are in
// plan order, and the error is the first one in plan order.
func buildParts(store string, parts [][]ckpt, prev manifestFile) ([]builtPart, error) {
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
				out[i], errs[i] = buildPart(store, parts[i], prev)
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

func buildPart(store string, members []ckpt, prev manifestFile) (builtPart, error) {
	id := fingerprint(members)
	if slices.ContainsFunc(members, ckpt.hot) {
		id, err := hotPart(store, members, prev)
		if err != nil {
			return builtPart{}, err
		}
		p := builtPart{id: id, hot: true}
		if d, ok := readBase(store, id); ok && d.Image != id {
			p.image = d.Image
		}
		return p, nil
	}
	if _, err := os.Stat(partDir(store, id)); err != nil {
		if err := buildIndex(store, members, id, partDir(store, id)); err != nil {
			return builtPart{}, err
		}
	}
	return builtPart{id: id}, nil
}

// planParts groups sources that share a session into one component, so a
// session and every event version that can compete for its uids live in one
// part. A component with a hot source is a part of its own, rebuilt on each of
// its appends; the settled ones go to partShards cold parts by a hash of their
// least session id, rewritten only when a component settles into or leaves one.
func planParts(cks []ckpt) [][]ckpt {
	parent := make([]int, len(cks))
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
	for i, c := range cks {
		for _, s := range c.Sessions {
			if j, ok := owner[s]; ok {
				parent[find(i)] = find(j)
			} else {
				owner[s] = i
			}
		}
	}
	least := map[int]string{}
	hot := map[int]bool{}
	for i, c := range cks {
		r := find(i)
		k := c.Key
		if len(c.Sessions) > 0 {
			k = c.Sessions[0]
		}
		if cur, ok := least[r]; !ok || k < cur {
			least[r] = k
		}
		hot[r] = hot[r] || c.hot()
	}
	cold := map[uint32][]ckpt{}
	hotParts := map[string][]ckpt{}
	for i, c := range cks {
		r := find(i)
		if hot[r] {
			hotParts[least[r]] = append(hotParts[least[r]], c)
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(least[r]))
		s := h.Sum32() % partShards
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

// gcLogs removes log generations no checkpoint names.
func gcLogs(store string, cks []ckpt) error {
	keep := map[string]bool{}
	for _, c := range cks {
		keep[filepath.Base(logPath(store, c.Key, c.Gen, "light"))] = true
		keep[filepath.Base(logPath(store, c.Key, c.Gen, "heavy"))] = true
		keep[filepath.Base(ckptPath(store, c.Key))] = true
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

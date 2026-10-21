package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
}

const window = 64 << 10

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

const frameTarget = 64 << 10

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
	return os.Rename(tmp, path)
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
		SourceFile: s.src.Path, Author: s.key.Author, Machine: s.key.Machine,
		Path: filepath.ToSlash(s.key.Events("")) /* lake path order */}
	prev, has := readCkpt(store, key)
	resumed := false
	if has && prev.Parser == want.Parser && prev.Harness == want.Harness && prev.SourceName == want.SourceName &&
		prev.SourceFile == want.SourceFile && prev.ZstSize <= end {
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
		next.Gen = prev.Gen
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

	lw := &frameWriter{f: lightF, off: from.LightLen}
	hw := &frameWriter{f: heavyF, off: from.HeavyLen}
	rows := from.Rows
	state, err := parser.Parse(zr, normalize.SourceMeta{SourceName: s.src.SourceName, Harness: s.src.Harness, SourceFile: s.src.Path},
		from.State, func(ev *schema.Event) error {
			r := rowFrom(ev)
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
	f   *os.File
	off int64
	buf []byte
	n   int
}

var encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(true))

func (w *frameWriter) add(rec []byte) (int64, int, error) {
	if len(w.buf) > 0 && len(w.buf)+len(rec) > frameTarget {
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
	comp := encoder.EncodeAll(w.buf, make([]byte, 8, 8+len(w.buf)/3))
	binary.LittleEndian.PutUint32(comp[0:4], uint32(len(comp)-8))
	binary.LittleEndian.PutUint32(comp[4:8], uint32(w.n))
	if _, err := w.f.Write(comp); err != nil {
		return err
	}
	w.off += int64(len(comp))
	w.buf, w.n = w.buf[:0], 0
	return nil
}

var decoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))

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

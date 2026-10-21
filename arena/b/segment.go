package main

// A segment is published as an immutable directory containing compressed
// metadata, independently compressed heavy blocks, and a front-coded index.
import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/klauspost/compress/zstd"
)

type heavyValue struct{ Text, Payload, Tokens string }
type heavyBlock struct {
	values []heavyValue
	bytes  int
}
type segment struct {
	path     string
	terms    []termEntry
	postings []byte
	mapping  []uint32
	pending  []*event
}
type termEntry struct {
	term         string
	offset, size uint32
}
type segmentWriter struct {
	path        string
	meta, heavy *os.File
	zw          *zstd.Encoder
	enc         *gob.Encoder
	codec       *zstd.Encoder
	records     []*event
	values      []heavyValue
	bytes       int
	count       uint32
	terms       map[string][]posting
}

func newSegmentWriter(dir string) (*segmentWriter, error) {
	path, e := os.MkdirTemp(dir, ".segment-")
	if e != nil {
		return nil, e
	}
	w := &segmentWriter{path: path, terms: map[string][]posting{}}
	w.meta, e = os.Create(filepath.Join(path, "meta.zst"))
	if e != nil {
		return nil, e
	}
	w.heavy, e = os.Create(filepath.Join(path, "heavy"))
	if e != nil {
		w.abort()
		return nil, e
	}
	w.zw, e = zstd.NewWriter(w.meta, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if e != nil {
		w.abort()
		return nil, e
	}
	w.enc = gob.NewEncoder(w.zw)
	w.codec, e = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if e != nil {
		w.abort()
		return nil, e
	}
	return w, nil
}
func (w *segmentWriter) abort() {
	if w.zw != nil {
		w.zw.Close()
	}
	if w.codec != nil {
		w.codec.Close()
	}
	if w.meta != nil {
		w.meta.Close()
	}
	if w.heavy != nil {
		w.heavy.Close()
	}
	os.RemoveAll(w.path)
}
func (w *segmentWriter) emit(ev *schema.Event) error {
	e := &event{Event: *ev}
	var payload struct {
		Emission uint32  `json:"moraine_emission_index"`
		IO       *toolIO `json:"moraine_tool_io"`
	}
	_ = json.Unmarshal([]byte(ev.PayloadJSON), &payload)
	e.Emission = payload.Emission
	if e.ToolPhase == "request" {
		e.IO = payload.IO
	}
	hash := sha256.Sum256([]byte(ev.TextContent))
	e.TextHash = binary.LittleEndian.Uint64(hash[:8])
	e.Flags = searchFlags(e)
	freq := map[string]uint32{}
	for _, t := range tokenRe.FindAllString(strings.ToLower(ev.TextContent), -1) {
		if len(t) >= 2 && len(t) <= 64 {
			freq[t]++
			e.Length++
		}
	}
	for t, tf := range freq {
		ps := w.terms[t]
		if ps == nil {
			t = strings.Clone(t)
		}
		w.terms[t] = append(ps, posting{w.count, tf})
	}
	w.count++
	w.records = append(w.records, e)
	w.values = append(w.values, heavyValue{ev.TextContent, ev.PayloadJSON, ev.TokenUsageJSON})
	w.bytes += len(ev.TextContent) + len(ev.PayloadJSON) + len(ev.TokenUsageJSON)
	e.TextContent = ""
	e.PayloadJSON = ""
	e.TokenUsageJSON = ""
	if ev.EventKind == "session_meta" || ev.SourceName == "omp" && (payloadString(ev.PayloadJSON, "type") == "title" || payloadString(ev.PayloadJSON, "type") == "title_change") {
		e.PayloadJSON = ev.PayloadJSON
	}
	if w.bytes >= 256<<10 || len(w.records) >= 128 {
		return w.flush()
	}
	return nil
}
func (w *segmentWriter) flush() error {
	if len(w.records) == 0 {
		return nil
	}
	var plain bytes.Buffer
	if e := gob.NewEncoder(&plain).Encode(w.values); e != nil {
		return e
	}
	compressed := w.codec.EncodeAll(plain.Bytes(), nil)
	offset, e := w.heavy.Seek(0, io.SeekCurrent)
	if e != nil {
		return e
	}
	if e = binary.Write(w.heavy, binary.LittleEndian, uint32(len(compressed))); e != nil {
		return e
	}
	if _, e = w.heavy.Write(compressed); e != nil {
		return e
	}
	for i, ev := range w.records {
		ev.HeavyOffset = uint64(offset)
		ev.HeavyIndex = uint32(i)
		if e = w.enc.Encode(ev); e != nil {
			return e
		}
	}
	clear(w.records)
	clear(w.values)
	w.records = w.records[:0]
	w.values = w.values[:0]
	w.bytes = 0
	return nil
}
func putU(w io.Writer, n uint64) error {
	var b [10]byte
	k := binary.PutUvarint(b[:], n)
	_, e := w.Write(b[:k])
	return e
}
func (w *segmentWriter) finish() (string, error) {
	if e := w.flush(); e != nil {
		w.abort()
		return "", e
	}
	if e := w.zw.Close(); e != nil {
		w.abort()
		return "", e
	}
	w.codec.Close()
	if e := w.meta.Sync(); e != nil {
		w.abort()
		return "", e
	}
	if e := w.heavy.Sync(); e != nil {
		w.abort()
		return "", e
	}
	w.meta.Close()
	w.heavy.Close()
	terms := make([]string, 0, len(w.terms))
	for t := range w.terms {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	f, e := os.Create(filepath.Join(w.path, "terms"))
	if e != nil {
		w.abort()
		return "", e
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	writeErr := func() error {
		if _, e := bw.WriteString("MNTERM02"); e != nil {
			return e
		}
		if e := putU(bw, uint64(len(terms))); e != nil {
			return e
		}
		prev := ""
		for _, t := range terms {
			prefix := 0
			for prefix < len(prev) && prefix < len(t) && prev[prefix] == t[prefix] {
				prefix++
			}
			if e := putU(bw, uint64(prefix)); e != nil {
				return e
			}
			if e := putU(bw, uint64(len(t)-prefix)); e != nil {
				return e
			}
			if _, e := bw.WriteString(t[prefix:]); e != nil {
				return e
			}
			var postings bytes.Buffer
			last := uint32(0)
			for _, p := range w.terms[t] {
				putU(&postings, uint64(p.Doc-last))
				putU(&postings, uint64(p.TF))
				last = p.Doc
			}
			if e := putU(bw, uint64(postings.Len())); e != nil {
				return e
			}
			if _, e := bw.Write(postings.Bytes()); e != nil {
				return e
			}
			prev = t
		}
		return bw.Flush()
	}()
	if writeErr == nil {
		writeErr = f.Sync()
	}
	ce := f.Close()
	if writeErr == nil {
		writeErr = ce
	}
	if writeErr != nil {
		w.abort()
		return "", writeErr
	}
	if e = syncDir(w.path); e != nil {
		w.abort()
		return "", e
	}
	name := strings.TrimPrefix(filepath.Base(w.path), ".")
	if e = os.Rename(w.path, filepath.Join(filepath.Dir(w.path), name)); e != nil {
		w.abort()
		return "", e
	}
	return name, syncDir(filepath.Dir(w.path))
}
func loadTerms(path string) ([]termEntry, []byte, error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, nil, e
	}
	if len(raw) < 8 || string(raw[:8]) != "MNTERM02" {
		return nil, nil, fmt.Errorf("invalid term index")
	}
	r := bytes.NewReader(raw[8:])
	n, e := binary.ReadUvarint(r)
	if e != nil || n > uint64(len(raw)) {
		return nil, nil, fmt.Errorf("invalid term count")
	}
	terms := make([]termEntry, 0, n)
	var postings []byte
	prev := ""
	for i := uint64(0); i < n; i++ {
		prefix, e := binary.ReadUvarint(r)
		if e != nil || prefix > uint64(len(prev)) {
			return nil, nil, fmt.Errorf("invalid term prefix")
		}
		size, e := binary.ReadUvarint(r)
		if e != nil || size > uint64(r.Len()) {
			return nil, nil, fmt.Errorf("invalid term length")
		}
		suffix := make([]byte, int(size))
		if _, e = io.ReadFull(r, suffix); e != nil {
			return nil, nil, e
		}
		t := prev[:prefix] + string(suffix)
		count, e := binary.ReadUvarint(r)
		if e != nil || count > uint64(r.Len()) {
			return nil, nil, fmt.Errorf("invalid posting length")
		}
		offset := len(postings)
		postings = append(postings, make([]byte, int(count))...)
		if _, e = io.ReadFull(r, postings[offset:]); e != nil {
			return nil, nil, e
		}
		terms = append(terms, termEntry{t, uint32(offset), uint32(count)})
		prev = t
	}
	return terms, postings, nil
}
func (s *segment) lookup(term string) []byte {
	i := sort.Search(len(s.terms), func(i int) bool { return s.terms[i].term >= term })
	if i == len(s.terms) || s.terms[i].term != term {
		return nil
	}
	t := s.terms[i]
	return s.postings[t.offset : t.offset+t.size]
}
func (s *segment) each(raw []byte, fn func(uint32, uint32)) {
	var doc uint32
	for len(raw) > 0 {
		gap, n := binary.Uvarint(raw)
		if n <= 0 {
			panic("corrupt postings")
		}
		raw = raw[n:]
		tf, n := binary.Uvarint(raw)
		if n <= 0 {
			panic("corrupt postings")
		}
		raw = raw[n:]
		doc += uint32(gap)
		global := s.mapping[doc]
		if global != ^uint32(0) {
			fn(global, uint32(tf))
		}
	}
}
func (db *Store) heavyFor(e *event) (heavyValue, error) {
	key := heavyKey{e.segment, e.HeavyOffset}
	if block, ok := db.heavyCache[key]; ok {
		return block.values[e.HeavyIndex], nil
	}
	f, err := os.Open(filepath.Join(e.segment.path, "heavy"))
	if err != nil {
		return heavyValue{}, err
	}
	defer f.Close()
	if _, err = f.Seek(int64(e.HeavyOffset), io.SeekStart); err != nil {
		return heavyValue{}, err
	}
	var size uint32
	if err = binary.Read(f, binary.LittleEndian, &size); err != nil {
		return heavyValue{}, err
	}
	if size > 256<<20 {
		return heavyValue{}, fmt.Errorf("invalid heavy block size")
	}
	b := make([]byte, size)
	if _, err = io.ReadFull(f, b); err != nil {
		return heavyValue{}, err
	}
	plain, err := db.decoder.DecodeAll(b, nil)
	if err != nil {
		return heavyValue{}, err
	}
	var values []heavyValue
	if err = gob.NewDecoder(bytes.NewReader(plain)).Decode(&values); err != nil {
		return heavyValue{}, err
	}
	if int(e.HeavyIndex) >= len(values) {
		return heavyValue{}, fmt.Errorf("invalid heavy row index")
	}
	// Bound retained decoded payloads. A large singleton is returned uncached.
	if len(plain) <= 8<<20 {
		for db.heavyBytes+len(plain) > 8<<20 && len(db.heavyOrder) > 0 {
			k := db.heavyOrder[0]
			db.heavyOrder = db.heavyOrder[1:]
			db.heavyBytes -= db.heavyCache[k].bytes
			delete(db.heavyCache, k)
		}
		db.heavyCache[key] = heavyBlock{values, len(plain)}
		db.heavyOrder = append(db.heavyOrder, key)
		db.heavyBytes += len(plain)
	}
	return values[e.HeavyIndex], nil
}

type heavyKey struct {
	segment *segment
	offset  uint64
}

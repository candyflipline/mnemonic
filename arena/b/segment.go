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
	"syscall"

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/klauspost/compress/zstd"
)

type heavyValue struct {
	Text, Payload, Tokens string
	Buckets               map[string]uint64
	Native                map[string]float64
}
type heavyBlock struct {
	values []heavyValue
	bytes  int
}
type segment struct {
	pathIndex
	path     string
	heavy    []byte
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
	tier        int
	path        string
	codecs      *segmentCodecs
	meta, heavy *os.File
	heavyBuffer *bufio.Writer
	heavyOffset uint64
	zw          *zstd.Encoder
	enc         *gob.Encoder
	codec       *zstd.Encoder
	records     []*event
	values      []heavyValue
	bytes       int
	inputBytes  int
	count       uint32
	terms       map[string][]posting
}

type segmentCodecs struct {
	meta, block *zstd.Encoder
	plain       bytes.Buffer
	compressed  []byte
}

func newSegmentCodecs() (*segmentCodecs, error) {
	meta, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if e != nil {
		return nil, e
	}
	block, e := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if e != nil {
		meta.Close()
		return nil, e
	}
	return &segmentCodecs{meta: meta, block: block}, nil
}
func (c *segmentCodecs) close() {
	if c != nil {
		c.meta.Close()
		c.block.Close()
	}
}
func newSegmentWriter(dir string, codecs *segmentCodecs) (*segmentWriter, error) {
	path, e := os.MkdirTemp(dir, ".segment-")
	if e != nil {
		return nil, e
	}
	w := &segmentWriter{path: path, terms: map[string][]posting{}, codecs: codecs}
	w.meta, e = os.Create(filepath.Join(path, "meta.zst"))
	if e != nil {
		w.abort()
		return nil, e
	}
	w.heavy, e = os.Create(filepath.Join(path, "heavy"))
	if e != nil {
		w.abort()
		return nil, e
	}
	w.heavyBuffer = bufio.NewWriterSize(w.heavy, 64<<10)
	w.zw, w.codec = codecs.meta, codecs.block
	w.zw.Reset(w.meta)
	w.enc = gob.NewEncoder(w.zw)
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
	e := packEvent(ev)
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
	text := strings.ToLower(ev.TextContent)
	for start := 0; start < len(text); {
		for start < len(text) && !asciiTokenByte(text[start]) {
			start++
		}
		end := start
		for end < len(text) && asciiTokenByte(text[end]) {
			end++
		}
		if end-start >= 2 && end-start <= 64 {
			freq[text[start:end]]++
			e.Length++
		}
		start = end
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
	w.values = append(w.values, heavyValue{ev.TextContent, ev.PayloadJSON, ev.TokenUsageJSON, ev.TokenUsageBuckets, ev.TokenUsageNativeUnits})
	w.bytes += len(ev.TextContent) + len(ev.PayloadJSON) + len(ev.TokenUsageJSON)
	w.inputBytes += len(ev.TextContent) + len(ev.PayloadJSON) + len(ev.TokenUsageJSON)

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
	plain := &w.codecs.plain
	plain.Reset()
	if e := gob.NewEncoder(plain).Encode(w.values); e != nil {
		return e
	}
	compressed := w.codec.EncodeAll(plain.Bytes(), w.codecs.compressed[:0])
	w.codecs.compressed = compressed
	offset := w.heavyOffset
	if e := binary.Write(w.heavyBuffer, binary.LittleEndian, uint32(len(compressed))); e != nil {
		return e
	}
	if _, e := w.heavyBuffer.Write(compressed); e != nil {
		return e
	}
	w.heavyOffset += uint64(4 + len(compressed))
	for i, ev := range w.records {
		ev.HeavyOffset = uint64(offset)
		ev.HeavyIndex = uint32(i)
		if e := w.enc.Encode(ev); e != nil {
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
	if e := w.heavyBuffer.Flush(); e != nil {
		w.abort()
		return "", e
	}
	if e := syscall.Fsync(int(w.meta.Fd())); e != nil {
		w.abort()
		return "", e
	}
	if e := syscall.Fsync(int(w.heavy.Fd())); e != nil {
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
		writeErr = syscall.Fsync(int(f.Fd()))
	}
	ce := f.Close()
	if writeErr == nil {
		writeErr = ce
	}
	if writeErr != nil {
		w.abort()
		return "", writeErr
	}
	if w.tier > 0 {
		tf, err := os.OpenFile(filepath.Join(w.path, "tier"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			w.abort()
			return "", err
		}
		_, err = tf.Write([]byte{byte(w.tier)})
		if err == nil {
			err = syscall.Fsync(int(tf.Fd()))
		}
		ce := tf.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			w.abort()
			return "", err
		}
	}
	if e = syncStagedDir(w.path); e != nil {
		w.abort()
		return "", e
	}
	name := strings.TrimPrefix(filepath.Base(w.path), ".")
	if e = os.Rename(w.path, filepath.Join(filepath.Dir(w.path), name)); e != nil {
		w.abort()
		return "", e
	}
	return name, syncStagedDir(filepath.Dir(w.path))
}

// Keep front coding in memory too. Only every sixteenth term is materialized
// as a fence key. Lookup decodes at most sixteen <=64-byte ASCII tokens on stack.
const termFence = 16

func readUAt(raw []byte, pos *int) (uint64, error) {
	if *pos >= len(raw) {
		return 0, io.ErrUnexpectedEOF
	}
	v, n := binary.Uvarint(raw[*pos:])
	if n <= 0 {
		return 0, fmt.Errorf("invalid index varint")
	}
	*pos += n
	return v, nil
}
func loadTerms(path string) (_ []termEntry, _ []byte, loadErr error) {
	raw, e := mapRead(path)
	if e != nil {
		return nil, nil, e
	}
	defer func() {
		if loadErr != nil {
			syscall.Munmap(raw)
		}
	}()
	if len(raw) < 8 || string(raw[:8]) != "MNTERM02" {
		return nil, nil, fmt.Errorf("invalid term index")
	}
	pos := 8
	n, e := readUAt(raw, &pos)
	if e != nil || n > uint64(len(raw)) {
		return nil, nil, fmt.Errorf("invalid term count")
	}
	fences := make([]termEntry, 0, (n+termFence-1)/termFence)
	var previous [64]byte
	previousLen := 0
	for i := uint64(0); i < n; i++ {
		start := pos
		prefix, e := readUAt(raw, &pos)
		if e != nil || prefix > uint64(previousLen) {
			return nil, nil, fmt.Errorf("invalid term prefix")
		}
		size, e := readUAt(raw, &pos)
		if e != nil || prefix+size > 64 || size > uint64(len(raw)-pos) {
			return nil, nil, fmt.Errorf("invalid term suffix")
		}
		copy(previous[prefix:], raw[pos:pos+int(size)])
		previousLen = int(prefix + size)
		pos += int(size)
		if i%termFence == 0 {
			fences = append(fences, termEntry{string(previous[:previousLen]), uint32(start), uint32(min(uint64(termFence), n-i))})
		}
		count, e := readUAt(raw, &pos)
		if e != nil || count > uint64(len(raw)-pos) {
			return nil, nil, fmt.Errorf("invalid posting length")
		}
		pos += int(count)
	}
	if pos != len(raw) {
		return nil, nil, fmt.Errorf("trailing term index bytes")
	}
	return fences, raw, nil
}
func (s *segment) lookup(term string) []byte {
	i := sort.Search(len(s.terms), func(i int) bool { return s.terms[i].term > term }) - 1
	if i < 0 {
		return nil
	}
	fence := s.terms[i]
	pos := int(fence.offset)
	var decoded [64]byte
	length := copy(decoded[:], fence.term)
	for j := uint32(0); j < fence.size; j++ {
		prefix, _ := readUAt(s.postings, &pos)
		size, _ := readUAt(s.postings, &pos)
		if j != 0 {
			copy(decoded[prefix:], s.postings[pos:pos+int(size)])
			length = int(prefix + size)
		}
		pos += int(size)
		count, _ := readUAt(s.postings, &pos)
		cmp := bytes.Compare(decoded[:length], []byte(term))
		if cmp == 0 {
			return s.postings[pos : pos+int(count)]
		}
		if cmp > 0 {
			return nil
		}
		pos += int(count)
	}
	return nil
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
	raw := e.segment.heavy
	offset := e.HeavyOffset
	if offset > uint64(len(raw)) || uint64(len(raw))-offset < 4 {
		return heavyValue{}, fmt.Errorf("invalid heavy block offset")
	}
	size := binary.LittleEndian.Uint32(raw[offset : offset+4])
	offset += 4
	if size > 256<<20 || uint64(size) > uint64(len(raw))-offset {
		return heavyValue{}, fmt.Errorf("invalid heavy block size")
	}
	b := raw[offset : offset+uint64(size)]
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

func asciiTokenByte(b byte) bool { return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' }

// Staged files use ordinary fsync. The manifest's File.Sync and parent-directory
// Sync form the full durability barriers before and after publication. On macOS
// File.Sync is F_FULLFSYNC, so issuing it for every staged file needlessly repeats
// a device-cache flush. All staged files and the manifest share this filesystem.
func syncStagedDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return syscall.Fsync(int(f.Fd()))
}

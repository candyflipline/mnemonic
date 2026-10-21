package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type loc struct {
	src   uint16
	idx   uint16
	frame uint64
}

type built struct {
	row *Row
	loc loc
	seq int64
}

// fingerprint names the set of checkpoints an index was built from.
func fingerprint(cks []ckpt) string {
	h := sha256.New()
	for _, c := range cks {
		fmt.Fprintf(h, "%s %d %d %d %d\n", c.Key, c.Gen, c.LightLen, c.HeavyLen, c.Rows)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type indexMeta struct {
	Fingerprint string   `json:"fingerprint"`
	Docs        int      `json:"docs"`
	ScoredDocs  uint64   `json:"scored_docs"`
	TotalLen    uint64   `json:"total_len"`
	Sources     []string `json:"sources"`
	Gens        []int    `json:"gens"`
	Harnesses   []string `json:"harnesses"`
	SourceNames []string `json:"source_names"`
	Sessions    int      `json:"sessions"`
	Turns       int      `json:"turns"`
	Terms       int      `json:"terms"`
}

func loadSourceRows(store string, si int, c ckpt) ([]built, error) {
	f, err := os.Open(logPath(store, c.Key, c.Gen, "light"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []built
	var scratch []byte
	var seq int64
	for off := int64(0); off < c.LightLen; {
		recs, next, err := readFrame(f, off, scratch)
		if err != nil {
			return nil, err
		}
		for i, rec := range recs {
			r := new(Row)
			if _, err := r.decode(rec); err != nil {
				return nil, err
			}
			out = append(out, built{row: r, loc: loc{src: uint16(si), idx: uint16(i), frame: uint64(off)}, seq: seq})
			seq++
		}
		off = next
	}
	return out, nil
}

func buildIndex(store string, cks []ckpt, fp string, sources []byte) error {
	sort.Slice(cks, func(i, j int) bool { return cks[i].Key < cks[j].Key })
	type winner struct {
		b    built
		path string
	}
	win := map[string]*winner{}
	for si, c := range cks {
		rows, err := loadSourceRows(store, si, c)
		if err != nil {
			return fmt.Errorf("%s: %w", c.Key, err)
		}
		local := map[string]int{}
		for i := range rows {
			r := rows[i].row
			if j, ok := local[r.UID]; ok && r.Version < rows[j].row.Version {
				continue
			}
			local[r.UID] = i
		}
		for _, i := range local {
			b := rows[i]
			w, ok := win[b.row.UID]
			if !ok || b.row.Version > w.b.row.Version || (b.row.Version == w.b.row.Version && c.Path < w.path) {
				win[b.row.UID] = &winner{b: b, path: c.Path}
			}
		}
	}
	bySession := map[string][]built{}
	for _, w := range win {
		bySession[w.b.row.Session] = append(bySession[w.b.row.Session], w.b)
	}
	win = nil
	sessionIDs := make([]string, 0, len(bySession))
	for s := range bySession {
		sessionIDs = append(sessionIDs, s)
	}
	sort.Strings(sessionIDs)

	harnesses, sourceNames := dict{}, dict{}
	var docs []built
	type sess struct {
		start, end  uint32
		first, last int64
		completed   bool
	}
	type turn struct {
		session, seq, count uint32
		completed           bool
		terminal            int64
		terminalOrder       int
	}
	heavy := newHeavyReader(store, cks)
	defer heavy.close()
	var lsRecs []lsRec
	var sessions []sess
	var turns []turn
	turnSeqs := []uint32{}
	ordinals := []uint32{}
	for si, sid := range sessionIDs {
		evs := bySession[sid]
		sort.Slice(evs, func(i, j int) bool { return orderLess(evs[i].row, evs[j].row) })
		start := uint32(len(docs))
		users := uint32(0)
		perTurn := map[uint32]*turn{}
		s := sess{start: start}
		for k, b := range evs {
			r := b.row
			if r.ActorKind == "user" && r.EventKind == "message" {
				users++
			}
			seq := uint32(r.TurnIndex)
			if seq == 0 {
				seq = max(1, users)
			}
			t := perTurn[seq]
			if t == nil {
				t = &turn{session: uint32(si), seq: seq, terminal: -1}
				perTurn[seq] = t
			}
			t.count++
			if r.PayloadType == "task_complete" {
				t.completed = true
				s.completed = true
			}
			if r.PayloadType == "task_complete" || r.PayloadType == "turn_aborted" {
				t.terminal = int64(len(docs))
			}
			turnSeqs = append(turnSeqs, seq)
			ordinals = append(ordinals, t.count)
			ms := timeMs(r)
			if k == 0 || ms < s.first {
				s.first = ms
			}
			if k == 0 || ms > s.last {
				s.last = ms
			}
			docs = append(docs, b)
		}
		s.end = uint32(len(docs))
		sessions = append(sessions, s)
		lsRecs = append(lsRecs, lsAggregate(evs, turnSeqs[start:], heavy.payload))
		seqs := make([]uint32, 0, len(perTurn))
		for q := range perTurn {
			seqs = append(seqs, q)
		}
		slices.Sort(seqs)
		for _, q := range seqs {
			turns = append(turns, *perTurn[q])
		}
		delete(bySession, sid)
	}

	dir := filepath.Join(store, "index.tmp")
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	n := len(docs)
	docCol := make([]byte, n*docWidth)
	var scoredDocs, totalLen uint64
	postings := map[string][]uint64{}
	uids := make([]uint64, 0, 2*n)
	var faExact []faEntry
	var faTio []uint32
	var faBlob []byte
	var faOffs []uint32
	tfs := map[string]uint32{}
	for d, b := range docs {
		r := b.row
		clear(tfs)
		dl := tokenize(r.Text, func(t string) { tfs[t]++ })
		if dl > 0 {
			scoredDocs++
			totalLen += uint64(dl)
			for t, tf := range tfs {
				postings[t] = append(postings[t], uint64(d)<<16|uint64(min(tf, 65535)))
			}
		}
		rec := docCol[d*docWidth : (d+1)*docWidth]
		binary.LittleEndian.PutUint64(rec[0:], b.loc.frame)
		binary.LittleEndian.PutUint16(rec[8:], b.loc.idx)
		binary.LittleEndian.PutUint16(rec[10:], b.loc.src)
		binary.LittleEndian.PutUint16(rec[12:], uint16(searchFlags(r)))
		rec[14] = byte(harnesses.id(r.Harness))
		rec[15] = byte(sourceNames.id(r.SourceName))
		binary.LittleEndian.PutUint32(rec[16:], uint32(sort.Search(len(sessions), func(i int) bool { return sessions[i].end > uint32(d) })))
		binary.LittleEndian.PutUint32(rec[20:], turnSeqs[d])
		binary.LittleEndian.PutUint32(rec[24:], ordinals[d])
		binary.LittleEndian.PutUint32(rec[28:], uint32(dl))
		binary.LittleEndian.PutUint64(rec[32:], uint64(timeMs(r)))
		uids = append(uids, uidHash(r.UID), uint64(d))
		if r.ToolPhase == "request" {
			if r.ProjectID != "" && r.RepoRelPath != "" {
				faExact = append(faExact, faEntry{r.RepoRelPath, uint32(d)})
			}
			if r.Bits&bitTio != 0 {
				faTio = append(faTio, uint32(d))
				faOffs = append(faOffs, uint32(len(faBlob)))
				faBlob = append(faBlob, r.TioInput...)
			}
		}
	}
	faOffs = append(faOffs, uint32(len(faBlob)))
	docs = nil

	if heavy.err != nil {
		return heavy.err
	}
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), sources, 0o644); err != nil {
		return err
	}
	if err := writeLs(dir, lsRecs); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "docs"), docCol, 0o644); err != nil {
		return err
	}

	var sb []byte
	var sstr []byte
	for i, s := range sessions {
		sb = binary.LittleEndian.AppendUint32(sb, s.start)
		sb = binary.LittleEndian.AppendUint32(sb, s.end)
		sb = binary.LittleEndian.AppendUint64(sb, uint64(s.first))
		sb = binary.LittleEndian.AppendUint64(sb, uint64(s.last))
		c := byte(0)
		if s.completed {
			c = 1
		}
		sb = binary.LittleEndian.AppendUint32(sb, uint32(len(sstr)))
		sb = binary.LittleEndian.AppendUint32(sb, uint32(c))
		sstr = append(sstr, sessionIDs[i]...)
	}
	sb = binary.LittleEndian.AppendUint32(sb, uint32(len(sstr)))
	if err := os.WriteFile(filepath.Join(dir, "sessions"), sb, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions.str"), sstr, 0o644); err != nil {
		return err
	}
	var tb []byte
	for _, t := range turns {
		tb = binary.LittleEndian.AppendUint32(tb, t.session)
		tb = binary.LittleEndian.AppendUint32(tb, t.seq)
		tb = binary.LittleEndian.AppendUint32(tb, t.count)
		c := uint32(0)
		if t.completed {
			c = 1
		}
		tb = binary.LittleEndian.AppendUint32(tb, c)
		tb = binary.LittleEndian.AppendUint64(tb, uint64(t.terminal))
	}
	if err := os.WriteFile(filepath.Join(dir, "turns"), tb, 0o644); err != nil {
		return err
	}

	pairs := make([][2]uint64, len(uids)/2)
	for i := range pairs {
		pairs[i] = [2]uint64{uids[2*i], uids[2*i+1]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var ub []byte
	for _, p := range pairs {
		ub = binary.LittleEndian.AppendUint64(ub, p[0])
		ub = binary.LittleEndian.AppendUint32(ub, uint32(p[1]))
	}
	if err := os.WriteFile(filepath.Join(dir, "uids"), ub, 0o644); err != nil {
		return err
	}

	terms := make([]string, 0, len(postings))
	for t := range postings {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	var dictB, postB []byte
	var dictOff []byte
	for _, t := range terms {
		dictOff = binary.LittleEndian.AppendUint32(dictOff, uint32(len(dictB)))
		dictOff = binary.LittleEndian.AppendUint64(dictOff, uint64(len(postB)))
		dictB = append(dictB, t...)
		ps := postings[t]
		postB = binary.AppendUvarint(postB, uint64(len(ps)))
		prev := uint64(0)
		for _, p := range ps {
			d := p >> 16
			postB = binary.AppendUvarint(postB, d-prev)
			postB = binary.AppendUvarint(postB, p&0xffff)
			prev = d
		}
		delete(postings, t)
	}
	dictOff = binary.LittleEndian.AppendUint32(dictOff, uint32(len(dictB)))
	dictOff = binary.LittleEndian.AppendUint64(dictOff, uint64(len(postB)))
	for name, b := range map[string][]byte{"terms": dictB, "terms.off": dictOff, "postings": postB} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}

	sort.Slice(faExact, func(i, j int) bool {
		if faExact[i].rel != faExact[j].rel {
			return faExact[i].rel < faExact[j].rel
		}
		return faExact[i].doc < faExact[j].doc
	})
	var fe []byte
	for _, e := range faExact {
		fe = appendStr(fe, e.rel)
		fe = binary.LittleEndian.AppendUint32(fe, e.doc)
	}
	var ft []byte
	for i, d := range faTio {
		ft = binary.LittleEndian.AppendUint32(ft, d)
		ft = binary.LittleEndian.AppendUint32(ft, faOffs[i])
	}
	ft = binary.LittleEndian.AppendUint32(ft, 0)
	ft = binary.LittleEndian.AppendUint32(ft, faOffs[len(faOffs)-1])
	for name, b := range map[string][]byte{"fa.exact": fe, "fa.tio": ft, "fa.blob": faBlob} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}

	meta := indexMeta{Fingerprint: fp, Docs: n, ScoredDocs: scoredDocs, TotalLen: totalLen,
		Harnesses: harnesses.list, SourceNames: sourceNames.list, Sessions: len(sessions), Turns: len(turns), Terms: len(terms)}
	for _, c := range cks {
		meta.Sources = append(meta.Sources, c.Key)
		meta.Gens = append(meta.Gens, c.Gen)
	}
	mb, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o644); err != nil {
		return err
	}
	return commitIndex(store, dir)
}

type faEntry struct {
	rel string
	doc uint32
}

const docWidth = 40

// commitIndex fsyncs the new index dir, renames it to index.<n> and points
// CURRENT at it by rename; the previous index is removed after.
func commitIndex(store, dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	cur, _ := os.ReadFile(filepath.Join(store, "CURRENT"))
	n := 0
	if v, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(string(cur)), "index.")); err == nil {
		n = v + 1
	}
	name := "index." + strconv.Itoa(n)
	os.RemoveAll(filepath.Join(store, name))
	if err := os.Rename(dir, filepath.Join(store, name)); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(store, "CURRENT"), []byte(name+"\n")); err != nil {
		return err
	}
	if old := strings.TrimSpace(string(cur)); old != "" && old != name {
		os.RemoveAll(filepath.Join(store, old))
	}
	return nil
}

type dict struct {
	list []string
	m    map[string]int
}

func (d *dict) id(s string) int {
	if d.m == nil {
		d.m = map[string]int{}
	}
	if i, ok := d.m[s]; ok {
		return i
	}
	d.m[s] = len(d.list)
	d.list = append(d.list, s)
	return len(d.list) - 1
}

func uidHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// timeMs is epoch_ms(event_time).
func timeMs(r *Row) int64 { return floorDiv(eventTimeUs(r), 1000) }

func eventTimeUs(r *Row) int64 {
	if r.Bits&bitTimeOK != 0 {
		return r.TimeUs
	}
	return ingestedAtUs
}

// ingestedAtUs stands in for the lake's ingested_at on rows without a
// parseable record_ts: one fixed stamp, later than any record_ts, so the
// order a clean run gives does not depend on when or in how many runs a
// source was parsed (CONTRACT-NOTES 4).
const ingestedAtUs int64 = 4102444800000000

// orderLess is the converge's session window order.
func orderLess(a, b *Row) bool {
	ta, tb := eventTimeUs(a), eventTimeUs(b)
	if ta != tb {
		return ta < tb
	}
	if a.SourceFile != b.SourceFile {
		return a.SourceFile < b.SourceFile
	}
	if a.Gen != b.Gen {
		return a.Gen < b.Gen
	}
	if a.Offset != b.Offset {
		return a.Offset < b.Offset
	}
	if a.LineNo != b.LineNo {
		return a.LineNo < b.LineNo
	}
	if a.Emission != b.Emission {
		return a.Emission < b.Emission
	}
	return a.UID < b.UID
}

// tokenize calls emit for every 2..64 char [a-z0-9_] run of lower(text) and
// returns the count.
func tokenize(text string, emit func(string)) int {
	ascii := true
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if !ascii {
		text = strings.ToLower(text)
	}
	n := 0
	start := -1
	var buf [64]byte
	flush := func(end int) {
		l := end - start
		if l >= 2 && l <= 64 {
			b := buf[:l]
			for i := 0; i < l; i++ {
				c := text[start+i]
				if c >= 'A' && c <= 'Z' {
					c += 32
				}
				b[i] = c
			}
			emit(string(b))
			n++
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || (c >= 'A' && c <= 'Z') {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			flush(i)
			start = -1
		}
	}
	if start >= 0 {
		flush(len(text))
	}
	return n
}

// heavyReader reads payload_json of rows at build, one frame at a time.
type heavyReader struct {
	files []*os.File
	key   frameKey
	recs  [][]byte
	err   error
}

func newHeavyReader(store string, cks []ckpt) *heavyReader {
	h := &heavyReader{key: frameKey{frame: ^uint64(0)}}
	for _, c := range cks {
		f, err := os.Open(logPath(store, c.Key, c.Gen, "heavy"))
		if err != nil && h.err == nil {
			h.err = err
		}
		h.files = append(h.files, f)
	}
	return h
}

func (h *heavyReader) payload(b built) string {
	if h.err != nil {
		return ""
	}
	k := frameKey{src: b.loc.src, frame: b.row.HeavyFrame}
	if k != h.key {
		recs, _, err := readFrame(h.files[k.src], int64(k.frame), nil)
		if err != nil {
			h.err = err
			return ""
		}
		h.key, h.recs = k, recs
	}
	rec := h.recs[b.row.HeavyIdx]
	l, n := binary.Uvarint(rec)
	return string(rec[n : n+int(l)])
}

func (h *heavyReader) close() {
	for _, f := range h.files {
		if f != nil {
			f.Close()
		}
	}
}

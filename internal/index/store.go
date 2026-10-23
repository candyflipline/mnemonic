package index

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// Part is a read-only view of one committed index. Its light and heavy logs
// hold the frames of its own docs, so it needs nothing outside its members.
type Part struct {
	meta                      indexMeta
	docs, sessions, sessStr   []byte
	turns, uids               []byte
	terms, termIdx, postings  []byte
	termFilter                []byte
	faExact, faTio, faBlob    []byte
	hotB, hotOff              []byte
	faTri, faTrip             []byte
	lightB, heavyB            []byte
	lightD, heavyD            []byte
	fc                        *frameCache
	maps                      [][]byte
	nSessions, nTurns, nTerms int
	scratch                   []byte
	ls                        []lsRec
	lsB                       []byte
}

// frameCache holds the last decoded light and heavy frames of every part of
// one DB, so batch memory is bounded by the cache sizes, not by the part count.
type frameCache struct {
	light      map[frameKey][]*Row
	lightOrder []frameKey
	heavy      map[frameKey][][]byte
	heavyOrder []frameKey
}

func newFrameCache() *frameCache {
	return &frameCache{light: map[frameKey][]*Row{}, heavy: map[frameKey][][]byte{}}
}

type frameKey struct {
	part  *Part
	frame uint64
}

func mmapFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() == 0 {
		return []byte{}, nil
	}
	return syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
}

// OpenPartDir opens a part dir.
func OpenPartDir(dir string) (*Part, error) {
	if d, ok, err := readDelta(dir); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return openDelta(dir, d)
	}
	mb, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var maps [][]byte
	st, err := openPart(mb, func(name string) ([]byte, error) {
		b, err := mmapFile(filepath.Join(dir, name))
		if err == nil {
			maps = append(maps, b)
		}
		return b, err
	})
	st.maps = maps
	if err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

func openPart(meta []byte, load func(name string) ([]byte, error)) (*Part, error) {
	st := &Part{fc: newFrameCache()}
	if err := json.Unmarshal(meta, &st.meta); err != nil {
		return st, err
	}
	for name, dst := range map[string]*[]byte{"docs": &st.docs, "sessions": &st.sessions, "sessions.str": &st.sessStr,
		"turns": &st.turns, "uids": &st.uids, "terms": &st.terms, "terms.idx": &st.termIdx, "terms.filter": &st.termFilter, "postings": &st.postings,
		"fa.exact": &st.faExact, "fa.tio": &st.faTio, "fa.blob": &st.faBlob,
		"hot": &st.hotB, "hot.off": &st.hotOff, "fa.tri": &st.faTri, "fa.trip": &st.faTrip,
		"light": &st.lightB, "heavy": &st.heavyB, "ls.json": &st.lsB} {
		b, err := load(name)
		if err != nil {
			return st, err
		}
		*dst = b
	}
	st.nSessions = st.meta.Sessions
	st.nTurns = st.meta.Turns
	st.nTerms = st.meta.Terms
	return st, nil
}

func (st *Part) Close() {
	for _, m := range st.maps {
		if len(m) > 0 {
			syscall.Munmap(m)
		}
	}
}

// segments reads a log held as an image and the delta appended after it.
type segments struct{ image, delta []byte }

func (s segments) ReadAt(p []byte, off int64) (int, error) {
	if n := int64(len(s.image)); off >= n {
		return bytes.NewReader(s.delta).ReadAt(p, off-n)
	}
	return bytes.NewReader(s.image).ReadAt(p, off)
}

func (st *Part) docRec(d int) []byte { return st.docs[d*docWidth : (d+1)*docWidth] }

func (st *Part) flags(d int) uint16 { return binary.LittleEndian.Uint16(st.docRec(d)[12:]) }
func (st *Part) harnessOf(d int) string {
	return st.meta.Harnesses[st.docRec(d)[14]]
}
func (st *Part) sourceNameOf(d int) string {
	return st.meta.SourceNames[st.docRec(d)[15]]
}
func (st *Part) sessionOf(d int) int  { return int(binary.LittleEndian.Uint32(st.docRec(d)[16:])) }
func (st *Part) turnSeq(d int) uint32 { return binary.LittleEndian.Uint32(st.docRec(d)[20:]) }
func (st *Part) ordinal(d int) uint32 { return binary.LittleEndian.Uint32(st.docRec(d)[24:]) }
func (st *Part) docLen(d int) uint32  { return binary.LittleEndian.Uint32(st.docRec(d)[28:]) }
func (st *Part) timeMsOf(d int) int64 { return int64(binary.LittleEndian.Uint64(st.docRec(d)[32:])) }

const sessWidth = 32

type sessInfo struct {
	id          string
	start, end  int
	first, last int64
	completed   bool
}

func (st *Part) session(i int) sessInfo {
	b := st.sessions[i*sessWidth:]
	so := binary.LittleEndian.Uint32(b[24:])
	se := binary.LittleEndian.Uint32(st.sessions[st.nSessions*sessWidth:])
	if i+1 < st.nSessions {
		se = binary.LittleEndian.Uint32(st.sessions[(i+1)*sessWidth+24:])
	}
	return sessInfo{
		id:        string(st.sessStr[so:se]),
		start:     int(binary.LittleEndian.Uint32(b[0:])),
		end:       int(binary.LittleEndian.Uint32(b[4:])),
		first:     int64(binary.LittleEndian.Uint64(b[8:])),
		last:      int64(binary.LittleEndian.Uint64(b[16:])),
		completed: binary.LittleEndian.Uint32(b[28:]) != 0,
	}
}

func (st *Part) sessionIndex(id string) (int, bool) {
	i := sort.Search(st.nSessions, func(i int) bool { return st.session(i).id >= id })
	if i < st.nSessions && st.session(i).id == id {
		return i, true
	}
	return 0, false
}

func (st *Part) sessionRange(id string) (int, int, bool) {
	i, ok := st.sessionIndex(id)
	if !ok {
		return 0, 0, false
	}
	s := st.session(i)
	return s.start, s.end, true
}

const turnWidth = 24

type turnInfo struct {
	count     int64
	completed bool
	terminal  int
}

func (st *Part) turn(session int, seq uint32) (turnInfo, bool) {
	at := func(i int) (uint32, uint32) {
		b := st.turns[i*turnWidth:]
		return binary.LittleEndian.Uint32(b), binary.LittleEndian.Uint32(b[4:])
	}
	i := sort.Search(st.nTurns, func(i int) bool {
		s, q := at(i)
		return s > uint32(session) || (s == uint32(session) && q >= seq)
	})
	if i >= st.nTurns {
		return turnInfo{}, false
	}
	if s, q := at(i); s != uint32(session) || q != seq {
		return turnInfo{}, false
	}
	b := st.turns[i*turnWidth:]
	return turnInfo{
		count:     int64(binary.LittleEndian.Uint32(b[8:])),
		completed: binary.LittleEndian.Uint32(b[12:]) != 0,
		terminal:  int(int64(binary.LittleEndian.Uint64(b[16:]))),
	}, true
}

func (st *Part) row(d int) (*Row, error) {
	rec := st.docRec(d)
	k := frameKey{part: st, frame: binary.LittleEndian.Uint64(rec[0:])}
	idx := int(binary.LittleEndian.Uint16(rec[8:]))
	rows, ok := st.fc.light[k]
	if !ok {
		recs, _, err := readFrame(segments{st.lightB, st.lightD}, int64(k.frame), st.scratch)
		if err != nil {
			return nil, err
		}
		rows = make([]*Row, len(recs))
		for i, b := range recs {
			r := new(Row)
			if _, err := r.decode(b); err != nil {
				return nil, err
			}
			rows[i] = r
		}
		st.fc.light[k] = rows
		st.fc.lightOrder = append(st.fc.lightOrder, k)
		if len(st.fc.lightOrder) > 48 {
			delete(st.fc.light, st.fc.lightOrder[0])
			st.fc.lightOrder = st.fc.lightOrder[1:]
		}
	}
	if idx >= len(rows) {
		return nil, fmt.Errorf("row %d: index past frame", d)
	}
	return rows[idx], nil
}

// heavyOf returns payload_json and token_usage_json of row r.
func (st *Part) heavyOf(r *Row) (string, string, error) {
	k := frameKey{part: st, frame: r.HeavyFrame}
	recs, ok := st.fc.heavy[k]
	if !ok {
		var err error
		recs, _, err = readFrame(segments{st.heavyB, st.heavyD}, int64(r.HeavyFrame), nil)
		if err != nil {
			return "", "", err
		}
		st.fc.heavy[k] = recs
		st.fc.heavyOrder = append(st.fc.heavyOrder, k)
		if len(st.fc.heavyOrder) > 16 {
			delete(st.fc.heavy, st.fc.heavyOrder[0])
			st.fc.heavyOrder = st.fc.heavyOrder[1:]
		}
	}
	if int(r.HeavyIdx) >= len(recs) {
		return "", "", fmt.Errorf("heavy index past frame")
	}
	b := recs[r.HeavyIdx]
	var out [2]string
	for i := range out {
		l, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < l {
			return "", "", errShort
		}
		out[i] = string(b[k : k+int(l)])
		b = b[k+int(l):]
	}
	return out[0], out[1], nil
}

func (st *Part) hydrate(r *navRow) error {
	row, err := st.row(r.doc)
	if err != nil {
		return err
	}
	p, t, err := st.heavyOf(row)
	if err != nil {
		return err
	}
	r.payloadJSON, r.tokenUsageJSON = p, t
	return nil
}

func (st *Part) hydrateKept(sc *sessionScan) error {
	var rows []*navRow
	if sc.target != nil {
		rows = append(rows, sc.target)
	}
	for _, t := range sc.turns {
		for _, r := range []*navRow{t.userInput, t.finalResponse} {
			if r != nil {
				rows = append(rows, r)
			}
		}
		rows = append(rows, t.selected...)
	}
	for _, r := range rows {
		if err := st.hydrate(r); err != nil {
			return err
		}
	}
	return nil
}

func navRowOf(r *Row, d int, turnSeq uint32) navRow {
	return navRow{
		sessionID:         r.Session,
		eventUID:          r.UID,
		eventUnixMs:       timeMs(r),
		eventTime:         formatRFC3339UTCMillis(timeMs(r)),
		sourceFile:        r.SourceFile,
		sourceRef:         r.SourceRef,
		eventClass:        r.EventKind,
		actorRole:         r.ActorKind,
		payloadType:       r.PayloadType,
		callID:            r.ToolCallID,
		name:              r.ToolName,
		phase:             r.Phase,
		itemID:            r.ItemID,
		sourceName:        r.SourceName,
		harness:           r.Harness,
		inferenceProvider: r.InfProv,
		textContent:       r.Text,
		tokenUsageBuckets: decodeUintMap(r.Buckets),
		tokenUsageNative:  decodeFloatMap(r.Native),
		endpointKind:      r.EndpointKind,
		turnSeq:           turnSeq,
		doc:               d,
	}
}

func (st *Part) sessionOfUID(uid string) (string, bool, error) {
	h := uidHash(uid)
	n := len(st.uids) / 12
	at := func(i int) uint64 { return binary.LittleEndian.Uint64(st.uids[i*12:]) }
	i := sort.Search(n, func(i int) bool { return at(i) >= h })
	for ; i < n && at(i) == h; i++ {
		d := int(binary.LittleEndian.Uint32(st.uids[i*12+8:]))
		r, err := st.row(d)
		if err != nil {
			return "", false, err
		}
		if r.UID == uid {
			return r.Session, true, nil
		}
	}
	return "", false, nil
}

// hot decodes doc d's hot record (see hotRow) and returns it with the hash of
// its text_content.
func (st *Part) hot(d int) (*Row, uint64, error) {
	lo := binary.LittleEndian.Uint64(st.hotOff[d*16:])
	th := binary.LittleEndian.Uint64(st.hotOff[d*16+8:])
	hi := binary.LittleEndian.Uint64(st.hotOff[(d+1)*16:])
	r := new(Row)
	if _, err := r.decode(st.hotB[lo:hi]); err != nil {
		return nil, 0, err
	}
	return r, th, nil
}

// faCandidates returns the ascending fa.blob entries that hold every
// trigram of needle, a superset of the entries containing it; ok is false
// when needle is too short to narrow anything.
func (st *Part) faCandidates(needle []byte) ([]int, bool) {
	if len(needle) < 3 {
		return nil, false
	}
	var out []int
	for j := 0; j+3 <= len(needle); j++ {
		k := trigram(needle[j:])
		n := len(st.faTri) / 8
		i := sort.Search(n, func(i int) bool { return binary.LittleEndian.Uint32(st.faTri[i*8:]) >= k })
		if i == n || binary.LittleEndian.Uint32(st.faTri[i*8:]) != k {
			return []int{}, true
		}
		p := st.faTrip[binary.LittleEndian.Uint32(st.faTri[i*8+4:]):]
		c, w := binary.Uvarint(p)
		p = p[w:]
		list := make([]int, 0, c)
		e := 0
		for range c {
			g, w := binary.Uvarint(p)
			p = p[w:]
			e += int(g)
			list = append(list, e)
		}
		if j == 0 {
			out = list
			continue
		}
		out = intersect(out, list)
		if len(out) == 0 {
			return out, true
		}
	}
	return out, true
}

func intersect(a, b []int) []int {
	out := a[:0]
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

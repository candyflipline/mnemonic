package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Store is a read-only view of one committed index plus the source logs it
// points into.
type Store struct {
	dir                         string
	meta                        indexMeta
	docs, sessions, sessStr     []byte
	turns, uids                 []byte
	terms, termsOff, postings   []byte
	faExact, faTio, faBlob      []byte
	light, heavy                []*os.File
	cache                       map[frameKey][]*Row
	cacheOrder                  []frameKey
	hcache                      map[frameKey][][]byte
	hcacheOrder                 []frameKey
	maps                        [][]byte
	nSessions, nTurns, nTerms   int
	scratch                     []byte
}

type frameKey struct {
	src   uint16
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

func openStore(store string) (*Store, error) {
	cur, err := os.ReadFile(filepath.Join(store, "CURRENT"))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(store, strings.TrimSpace(string(cur)))
	st := &Store{dir: dir, cache: map[frameKey][]*Row{}, hcache: map[frameKey][][]byte{}}
	mb, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(mb, &st.meta); err != nil {
		return nil, err
	}
	for name, dst := range map[string]*[]byte{"docs": &st.docs, "sessions": &st.sessions, "sessions.str": &st.sessStr,
		"turns": &st.turns, "uids": &st.uids, "terms": &st.terms, "terms.off": &st.termsOff, "postings": &st.postings,
		"fa.exact": &st.faExact, "fa.tio": &st.faTio, "fa.blob": &st.faBlob} {
		b, err := mmapFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		*dst = b
		st.maps = append(st.maps, b)
	}
	for i, key := range st.meta.Sources {
		l, err := os.Open(logPath(store, key, st.meta.Gens[i], "light"))
		if err != nil {
			return nil, err
		}
		h, err := os.Open(logPath(store, key, st.meta.Gens[i], "heavy"))
		if err != nil {
			return nil, err
		}
		st.light = append(st.light, l)
		st.heavy = append(st.heavy, h)
	}
	st.nSessions = st.meta.Sessions
	st.nTurns = st.meta.Turns
	st.nTerms = st.meta.Terms
	return st, nil
}

func (st *Store) docRec(d int) []byte { return st.docs[d*docWidth : (d+1)*docWidth] }

func (st *Store) flags(d int) uint16 { return binary.LittleEndian.Uint16(st.docRec(d)[12:]) }
func (st *Store) harnessOf(d int) string {
	return st.meta.Harnesses[st.docRec(d)[14]]
}
func (st *Store) sourceNameOf(d int) string {
	return st.meta.SourceNames[st.docRec(d)[15]]
}
func (st *Store) sessionOf(d int) int    { return int(binary.LittleEndian.Uint32(st.docRec(d)[16:])) }
func (st *Store) turnSeq(d int) uint32  { return binary.LittleEndian.Uint32(st.docRec(d)[20:]) }
func (st *Store) ordinal(d int) uint32  { return binary.LittleEndian.Uint32(st.docRec(d)[24:]) }
func (st *Store) docLen(d int) uint32   { return binary.LittleEndian.Uint32(st.docRec(d)[28:]) }
func (st *Store) timeMsOf(d int) int64  { return int64(binary.LittleEndian.Uint64(st.docRec(d)[32:])) }

const sessWidth = 32

type sessInfo struct {
	id          string
	start, end  int
	first, last int64
	completed   bool
}

func (st *Store) session(i int) sessInfo {
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

func (st *Store) sessionIndex(id string) (int, bool) {
	i := sort.Search(st.nSessions, func(i int) bool { return st.session(i).id >= id })
	if i < st.nSessions && st.session(i).id == id {
		return i, true
	}
	return 0, false
}

func (st *Store) sessionRange(id string) (int, int, bool) {
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

func (st *Store) turn(session int, seq uint32) (turnInfo, bool) {
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

func (st *Store) row(d int) (*Row, error) {
	rec := st.docRec(d)
	k := frameKey{src: binary.LittleEndian.Uint16(rec[10:]), frame: binary.LittleEndian.Uint64(rec[0:])}
	idx := int(binary.LittleEndian.Uint16(rec[8:]))
	rows, ok := st.cache[k]
	if !ok {
		recs, _, err := readFrame(st.light[k.src], int64(k.frame), st.scratch)
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
		st.cache[k] = rows
		st.cacheOrder = append(st.cacheOrder, k)
		if len(st.cacheOrder) > 48 {
			delete(st.cache, st.cacheOrder[0])
			st.cacheOrder = st.cacheOrder[1:]
		}
	}
	if idx >= len(rows) {
		return nil, fmt.Errorf("row %d: index past frame", d)
	}
	return rows[idx], nil
}

// heavyOf returns payload_json and token_usage_json of row r from source src.
func (st *Store) heavyOf(src uint16, r *Row) (string, string, error) {
	k := frameKey{src: src, frame: r.HeavyFrame}
	recs, ok := st.hcache[k]
	if !ok {
		var err error
		recs, _, err = readFrame(st.heavy[src], int64(r.HeavyFrame), nil)
		if err != nil {
			return "", "", err
		}
		st.hcache[k] = recs
		st.hcacheOrder = append(st.hcacheOrder, k)
		if len(st.hcacheOrder) > 16 {
			delete(st.hcache, st.hcacheOrder[0])
			st.hcacheOrder = st.hcacheOrder[1:]
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

func (st *Store) hydrate(r *navRow) error {
	row, err := st.row(r.doc)
	if err != nil {
		return err
	}
	src := binary.LittleEndian.Uint16(st.docRec(r.doc)[10:])
	p, t, err := st.heavyOf(src, row)
	if err != nil {
		return err
	}
	r.payloadJSON, r.tokenUsageJSON = p, t
	return nil
}

func (st *Store) hydrateKept(sc *sessionScan) error {
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

func (st *Store) sessionOfUID(uid string) (string, bool, error) {
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

package index

import (
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"os"
	"sort"
)

// A part is built in passes that keep no decoded row: the key pass reads every
// member once into a compact rowKey per winner, the doc pass decodes each
// winner's light record once in the part's order, and the section pass reads
// each session's winners back from the part's own logs.

// rowKey is what the key pass keeps of a row: what the winner rule and the
// part's order read, and where its light record is.
type rowKey struct {
	uid        string
	version    uint64
	timeUs     int64
	gen        uint64
	offset     uint64
	lineNo     uint64
	emission   uint64
	loc        rowLoc
	sourceFile uint32
	session    uint32
	src        uint16
}

// strTab interns strings to the dense ids rowKey carries.
type strTab struct {
	list []string
	ids  map[string]uint32
}

func (t *strTab) id(b []byte) uint32 {
	if id, ok := t.ids[string(b)]; ok {
		return id
	}
	if t.ids == nil {
		t.ids = map[string]uint32{}
	}
	id := uint32(len(t.list))
	t.list = append(t.list, string(b))
	t.ids[t.list[id]] = id
	return id
}

// ranks maps each id to its place in the strings' sorted order.
func (t *strTab) ranks() []uint32 {
	order := make([]uint32, len(t.list))
	for i := range order {
		order[i] = uint32(i)
	}
	sort.Slice(order, func(i, j int) bool { return t.list[order[i]] < t.list[order[j]] })
	rank := make([]uint32, len(order))
	for r, id := range order {
		rank[id] = uint32(r)
	}
	return rank
}

// keyPass folds the rows of every member into the winners' keys.
type keyPass struct {
	members  []member
	pos      keyPos
	win      map[string]int32
	keys     []rowKey
	files    strTab
	sessions strTab
	seen     map[authKey]bool
	last     authKey
}

type authKey struct {
	session uint32
	src     uint16
}

// newKeyPass starts a key pass over members whose rows total at most rows, the
// most winners there can be.
func newKeyPass(members []member, rows int) *keyPass {
	return &keyPass{members: members, pos: newKeyPos(), win: map[string]int32{}, keys: make([]rowKey, 0, rows),
		seen: map[authKey]bool{}, last: authKey{session: ^uint32(0)}}
}

// offer folds one light record of member src, by the winner rule offerRow
// holds: within a member the higher version wins, last on a tie; across
// members memberBeats.
func (p *keyPass) offer(src uint16, author string, rec []byte, loc rowLoc) error {
	f, err := p.pos.scan(rec)
	if err != nil {
		return err
	}
	k := rowKey{version: f.version, gen: f.gen, offset: f.offset, lineNo: f.lineNo, emission: f.emission,
		loc: loc, src: src, session: p.sessions.id(f.session), sourceFile: p.files.id(f.sourceFile)}
	k.timeUs = ingestedAtUs
	if f.bits&bitTimeOK != 0 {
		k.timeUs = f.timeUs
	}
	if len(f.session) > 0 && author != "" {
		if a := (authKey{k.session, src}); a != p.last {
			p.last = a
			p.seen[a] = true
		}
	}
	if at, ok := p.win[string(f.uid)]; ok {
		w := &p.keys[at]
		if w.src == src {
			if k.version < w.version {
				return nil
			}
		} else if !memberBeats(k.version, p.members[src], w.version, p.members[w.src]) {
			return nil
		}
		k.uid = w.uid
		*w = k
		return nil
	}
	k.uid = string(f.uid)
	p.win[k.uid] = int32(len(p.keys))
	p.keys = append(p.keys, k)
	return nil
}

// authorsBySession is authorsOf over every row offered: per session, the
// authors of the members that emitted a row of it, sorted and distinct.
func (p *keyPass) authorsBySession(cks []ckpt) map[string][]string {
	set := map[string]map[string]bool{}
	for a := range p.seen {
		sid := p.sessions.list[a.session]
		if set[sid] == nil {
			set[sid] = map[string]bool{}
		}
		set[sid][cks[a.src].Author] = true
	}
	out := make(map[string][]string, len(set))
	for sid, as := range set {
		out[sid] = sortedAuthors(as)
	}
	return out
}

// docLoc is what the doc pass needs of a winner: its session and where its
// light record is.
type docLoc struct {
	lf      int64
	session uint32
	li      uint16 // the record's index in its frame, holderBit set for a holder part's frame
	src     uint16
}

const holderBit = 1 << 15

func (d docLoc) rowLoc() rowLoc {
	return rowLoc{lf: d.lf, li: d.li &^ holderBit, holder: d.li&holderBit != 0}
}

// sorted returns the winners in the part's order, session by session (the
// session ids' byte order), each session's rows by orderLess, and frees the
// keys and the uid map.
func (p *keyPass) sorted() []docLoc {
	p.win = nil
	sess, files := p.sessions.ranks(), p.files.ranks()
	keys := p.keys
	p.keys = nil
	sort.Sort(byOrder{keys: keys, sess: sess, files: files})
	locs := make([]docLoc, len(keys))
	for i := range keys {
		k := &keys[i]
		locs[i] = docLoc{lf: k.loc.lf, session: k.session, li: k.loc.li, src: k.src}
		if k.loc.holder {
			locs[i].li |= holderBit
		}
	}
	return locs
}

type byOrder struct {
	keys        []rowKey
	sess, files []uint32
}

func (o byOrder) Len() int      { return len(o.keys) }
func (o byOrder) Swap(i, j int) { o.keys[i], o.keys[j] = o.keys[j], o.keys[i] }

// Less is orderLess over the keys' fields, after the session.
func (o byOrder) Less(i, j int) bool {
	a, b := &o.keys[i], &o.keys[j]
	if a.session != b.session {
		return o.sess[a.session] < o.sess[b.session]
	}
	switch {
	case a.timeUs != b.timeUs:
		return a.timeUs < b.timeUs
	case a.sourceFile != b.sourceFile:
		return o.files[a.sourceFile] < o.files[b.sourceFile]
	case a.gen != b.gen:
		return a.gen < b.gen
	case a.offset != b.offset:
		return a.offset < b.offset
	case a.lineNo != b.lineNo:
		return a.lineNo < b.lineNo
	case a.emission != b.emission:
		return a.emission < b.emission
	}
	return a.uid < b.uid
}

// keyPos is where the fields of a rowKey sit in a light record, found from
// Row's own field lists so a reorder there cannot desync the scan.
type keyPos struct {
	nStr, nInt                                   int
	uid, session, sourceFile                     int
	gen, lineNo, offset, version, emission, bits int
}

func newKeyPos() keyPos {
	var r Row
	strs, ints := r.strs(), r.ints()
	at := func(list []*uint64, p *uint64) int {
		for i, q := range list {
			if q == p {
				return i
			}
		}
		panic("keyPos: field not in Row.ints")
	}
	atStr := func(p *string) int {
		for i, q := range strs {
			if q == p {
				return i
			}
		}
		panic("keyPos: field not in Row.strs")
	}
	return keyPos{nStr: len(strs), nInt: len(ints),
		uid: atStr(&r.UID), session: atStr(&r.Session), sourceFile: atStr(&r.SourceFile),
		gen: at(ints, &r.Gen), lineNo: at(ints, &r.LineNo), offset: at(ints, &r.Offset),
		version: at(ints, &r.Version), emission: at(ints, &r.Emission), bits: at(ints, &r.Bits)}
}

// keyFields are the fields scan reads; the byte slices point into the record.
type keyFields struct {
	uid, session, sourceFile                     []byte
	timeUs                                       int64
	gen, lineNo, offset, version, emission, bits uint64
}

// scan reads the key fields of a light record without decoding the rest.
func (p keyPos) scan(b []byte) (f keyFields, err error) {
	for i := 0; i < p.nStr; i++ {
		n, k := binary.Uvarint(b)
		if k <= 0 || uint64(len(b)-k) < n {
			return f, errShort
		}
		switch i {
		case p.uid:
			f.uid = b[k : k+int(n)]
		case p.session:
			f.session = b[k : k+int(n)]
		case p.sourceFile:
			f.sourceFile = b[k : k+int(n)]
		}
		b = b[k+int(n):]
	}
	v, k := binary.Varint(b)
	if k <= 0 {
		return f, errShort
	}
	f.timeUs, b = v, b[k:]
	if _, k = binary.Varint(b); k <= 0 {
		return f, errShort
	}
	b = b[k:]
	for i := 0; i < p.nInt; i++ {
		v, k := binary.Uvarint(b)
		if k <= 0 {
			return f, errShort
		}
		b = b[k:]
		switch i {
		case p.gen:
			f.gen = v
		case p.lineNo:
			f.lineNo = v
		case p.offset:
			f.offset = v
		case p.version:
			f.version = v
		case p.emission:
			f.emission = v
		case p.bits:
			f.bits = v
		}
	}
	return f, nil
}

const (
	// lightFrames and sectionFrames are how many decoded light frames the doc
	// pass and the section pass keep.
	lightFrames   = 16
	sectionFrames = 4
)

type frameID struct {
	src    uint16
	holder bool
	off    int64
}

// frameLRU keeps the last decoded frames read, the bounded reader behind both
// passes that read light records back.
type frameLRU struct {
	max     int
	recs    map[frameID][][]byte
	order   []frameID
	decodes int
}

func newFrameLRU(max int) *frameLRU {
	return &frameLRU{max: max, recs: map[frameID][][]byte{}}
}

func (c *frameLRU) get(id frameID, load func() ([][]byte, error)) ([][]byte, error) {
	if recs, ok := c.recs[id]; ok {
		for i, o := range c.order {
			if o == id {
				copy(c.order[i:], c.order[i+1:])
				c.order[len(c.order)-1] = id
				break
			}
		}
		return recs, nil
	}
	recs, err := load()
	if err != nil {
		return nil, err
	}
	c.decodes++
	c.recs[id] = recs
	c.order = append(c.order, id)
	if len(c.order) > c.max {
		delete(c.recs, c.order[0])
		c.order = c.order[1:]
	}
	return recs, nil
}

// lightSource reads a winner's light record back from where the key pass
// found it: its holder part's light log or its member's.
type lightSource struct {
	store   string
	cks     []ckpt
	holders []*Part
	frames  *frameLRU
}

func (s *lightSource) record(src uint16, loc rowLoc) ([]byte, error) {
	id := frameID{src: src, holder: loc.holder, off: loc.lf}
	recs, err := s.frames.get(id, func() ([][]byte, error) {
		if loc.holder {
			h := s.holders[src]
			recs, _, err := readFrame(segments{h.lightB, h.lightD}, loc.lf, nil)
			if err != nil {
				return nil, fmt.Errorf("%s: holder %s: %w", s.cks[src].Key, h.meta.Fingerprint, err)
			}
			return recs, nil
		}
		c := s.cks[src]
		f, err := os.Open(logPath(s.store, c.Key, c.Gen, "light"))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		recs, _, err := readFrame(f, loc.lf, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Key, err)
		}
		return recs, nil
	})
	if err != nil {
		return nil, err
	}
	if int(loc.li) >= len(recs) {
		return nil, fmt.Errorf("%s: light index past frame", s.cks[src].Key)
	}
	return recs[loc.li], nil
}

// partView hands one session of the new part to the derived sections as the
// iterators they take, each row decoded from the part's own light log with
// its member's author. A yielded row is valid during its yield; payload reads
// the part's own heavy log.
type partView struct {
	acc        *partAcc
	cks        []ckpt
	docs       io.ReaderAt
	chunk      []byte
	chunkAt    int
	light      io.ReaderAt
	frames     *frameLRU
	heavy      *heavyReader
	start, end int
	err        error
}

// docChunk is how many doc records docRec reads at a time.
const docChunk = 1024

// docRec is doc d's record in the part's docs file.
func (v *partView) docRec(d int) ([]byte, error) {
	if v.chunk == nil || d < v.chunkAt || d >= v.chunkAt+len(v.chunk)/docWidth {
		n := min(docChunk, v.acc.nDocs-d)
		if v.chunk == nil {
			v.chunk = make([]byte, docChunk*docWidth)
		}
		v.chunk = v.chunk[:n*docWidth]
		if _, err := v.docs.ReadAt(v.chunk, int64(d)*docWidth); err != nil {
			return nil, err
		}
		v.chunkAt = d
	}
	return v.chunk[(d-v.chunkAt)*docWidth : (d-v.chunkAt+1)*docWidth], nil
}

func (v *partView) begin(session int) {
	s := v.acc.sessions[session]
	v.start, v.end = int(s.start), int(s.end)
}

// load decodes doc d into r and returns its turn seq.
func (v *partView) load(d int, r *Row) (uint32, bool) {
	rec, err := v.docRec(d)
	if err != nil {
		v.err = err
		return 0, false
	}
	lf, li := int64(binary.LittleEndian.Uint64(rec[0:])), int(binary.LittleEndian.Uint16(rec[8:]))
	src, seq := binary.LittleEndian.Uint16(rec[10:]), binary.LittleEndian.Uint32(rec[20:])
	recs, err := v.frames.get(frameID{off: lf}, func() ([][]byte, error) {
		recs, _, err := readFrame(v.light, lf, nil)
		return recs, err
	})
	if err == nil && li >= len(recs) {
		err = fmt.Errorf("doc %d: light index past frame", d)
	}
	if err == nil {
		*r = Row{}
		_, err = r.decode(recs[li])
	}
	if err != nil {
		v.err = err
		return 0, false
	}
	r.Author = v.cks[src].Author
	return seq, true
}

// withSeqs yields the session's rows as the parser wrote them, each with its
// turn seq beside it.
func (v *partView) withSeqs() iter.Seq2[*Row, uint32] {
	return func(yield func(*Row, uint32) bool) {
		var r Row
		for d := v.start; d < v.end; d++ {
			seq, ok := v.load(d, &r)
			if !ok || !yield(&r, seq) {
				return
			}
		}
	}
}

// rows yields the session's rows with TurnIndex set to the turn seq.
func (v *partView) rows() iter.Seq[*Row] {
	return func(yield func(*Row) bool) {
		var r Row
		for d := v.start; d < v.end; d++ {
			seq, ok := v.load(d, &r)
			if !ok {
				return
			}
			r.TurnIndex = uint64(seq)
			if !yield(&r) {
				return
			}
		}
	}
}

func (v *partView) payload(r *Row) string { return v.heavy.payload(built{row: r}) }

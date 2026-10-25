package index

import (
	"encoding/binary"
	"io"
	"slices"
	"sort"
)

type sessRec struct {
	start, end  uint32
	first, last int64
	completed   bool
}

type turnRec struct {
	session, seq, count uint32
	completed           bool
	terminal            int64
}

// partAcc accumulates the files of one part from its docs, fed one doc at a
// time in the part's order: session by session, each session's docs in event
// order. The four files that grow by a record per doc (hot, hot.off, docs and
// the file-attention blob) go to their writers as the docs arrive; everything
// else is held until finish writes it.
type partAcc struct {
	hot, hotOffW, docs, blob io.Writer

	harnesses, sourceNames dict
	sessionIDs             []string
	sessions               []sessRec
	turns                  []turnRec
	nDocs                  int
	scoredDocs, totalLen   uint64
	postings               map[string][]uint64
	tfs                    map[string]uint32
	pairs                  [][2]uint64
	faExact                []faEntry
	faTio, faOffs          []uint32
	blobLen                int
	tri                    triBuilder
	hotLen                 int
	hotBuf, offBuf         []byte
	rec                    [docWidth]byte

	users   uint32
	perTurn map[uint32]*turnRec
}

// newPartAcc starts a part of docs docs.
func newPartAcc(docs int, hot, hotOff, docRecs, blob io.Writer) *partAcc {
	return &partAcc{hot: hot, hotOffW: hotOff, docs: docRecs, blob: blob, pairs: make([][2]uint64, 0, docs),
		postings: map[string][]uint64{}, tfs: map[string]uint32{}, tri: newTriBuilder()}
}

// beginSession opens the next session; the previous one must be ended.
func (p *partAcc) beginSession(sid string) {
	p.sessionIDs = append(p.sessionIDs, sid)
	p.sessions = append(p.sessions, sessRec{start: uint32(p.nDocs)})
	p.users = 0
	p.perTurn = map[uint32]*turnRec{}
}

func (p *partAcc) endSession() {
	p.sessions[len(p.sessions)-1].end = uint32(p.nDocs)
	seqs := make([]uint32, 0, len(p.perTurn))
	for q := range p.perTurn {
		seqs = append(seqs, q)
	}
	slices.Sort(seqs)
	for _, q := range seqs {
		p.turns = append(p.turns, *p.perTurn[q])
	}
	p.perTurn = nil
}

// add appends r, the part's next doc, as stored at light frame lf index li of
// the part, and returns its turn seq.
func (p *partAcc) add(r *Row, src uint16, lf int64, li int) (uint32, error) {
	d := p.nDocs
	si := len(p.sessions) - 1
	s := &p.sessions[si]
	if r.ActorKind == "user" && r.EventKind == "message" {
		p.users++
	}
	seq := uint32(r.TurnIndex)
	if seq == 0 {
		seq = max(1, p.users)
	}
	t := p.perTurn[seq]
	if t == nil {
		t = &turnRec{session: uint32(si), seq: seq, terminal: -1}
		p.perTurn[seq] = t
	}
	t.count++
	if r.PayloadType == "task_complete" {
		t.completed = true
		s.completed = true
	}
	if r.PayloadType == "task_complete" || r.PayloadType == "turn_aborted" {
		t.terminal = int64(d)
	}
	ms := timeMs(r)
	if d == int(s.start) || ms < s.first {
		s.first = ms
	}
	if d == int(s.start) || ms > s.last {
		s.last = ms
	}

	p.offBuf = binary.LittleEndian.AppendUint64(p.offBuf[:0], uint64(p.hotLen))
	p.hotBuf = hotRow(*r).append(p.hotBuf[:0])
	if _, err := p.hot.Write(p.hotBuf); err != nil {
		return 0, err
	}
	p.hotLen += len(p.hotBuf)
	p.offBuf = binary.LittleEndian.AppendUint64(p.offBuf, textHash(r.Text))
	if _, err := p.hotOffW.Write(p.offBuf); err != nil {
		return 0, err
	}
	clear(p.tfs)
	dl := tokenize(r.Text, func(t string) { p.tfs[t]++ })
	if dl > 0 {
		p.scoredDocs++
		p.totalLen += uint64(dl)
		for t, tf := range p.tfs {
			p.postings[t] = append(p.postings[t], uint64(d)<<16|uint64(min(tf, 65535)))
		}
	}
	rec := p.rec[:]
	binary.LittleEndian.PutUint64(rec[0:], uint64(lf))
	binary.LittleEndian.PutUint16(rec[8:], uint16(li))
	binary.LittleEndian.PutUint16(rec[10:], src)
	binary.LittleEndian.PutUint16(rec[12:], uint16(searchFlags(r)))
	rec[14] = byte(p.harnesses.id(r.Harness))
	rec[15] = byte(p.sourceNames.id(r.SourceName))
	binary.LittleEndian.PutUint32(rec[16:], uint32(si))
	binary.LittleEndian.PutUint32(rec[20:], seq)
	binary.LittleEndian.PutUint32(rec[24:], t.count)
	binary.LittleEndian.PutUint32(rec[28:], uint32(dl))
	binary.LittleEndian.PutUint64(rec[32:], uint64(ms))
	if _, err := p.docs.Write(rec); err != nil {
		return 0, err
	}
	p.pairs = append(p.pairs, [2]uint64{uidHash(r.UID), uint64(d)})
	if r.ToolPhase == "request" {
		if r.ProjectID != "" && r.RepoRelPath != "" {
			p.faExact = append(p.faExact, faEntry{r.RepoRelPath, uint32(d)})
		}
		if r.Bits&bitTio != 0 {
			p.tri.add(len(p.faTio), r.TioInput)
			p.faTio = append(p.faTio, uint32(d))
			p.faOffs = append(p.faOffs, uint32(p.blobLen))
			if _, err := io.WriteString(p.blob, r.TioInput); err != nil {
				return 0, err
			}
			p.blobLen += len(r.TioInput)
		}
	}
	p.nDocs++
	return seq, nil
}

// finish writes every file the docs fed to add make, but for the four add
// streamed, through write, and returns the
// part's meta without its fingerprint and members.
func (p *partAcc) finish(write func(name string, b []byte) error) (indexMeta, error) {
	p.faOffs = append(p.faOffs, uint32(p.blobLen))
	if _, err := p.hotOffW.Write(binary.LittleEndian.AppendUint64(nil, uint64(p.hotLen))); err != nil {
		return indexMeta{}, err
	}

	var sb []byte
	var sstr []byte
	for i, s := range p.sessions {
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
		sstr = append(sstr, p.sessionIDs[i]...)
	}
	sb = binary.LittleEndian.AppendUint32(sb, uint32(len(sstr)))
	var tb []byte
	for _, t := range p.turns {
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
	for name, b := range map[string][]byte{"sessions": sb, "sessions.str": sstr, "turns": tb} {
		if err := write(name, b); err != nil {
			return indexMeta{}, err
		}
	}

	pairs := p.pairs
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var ub []byte
	for _, pr := range pairs {
		ub = binary.LittleEndian.AppendUint64(ub, pr[0])
		ub = binary.LittleEndian.AppendUint32(ub, uint32(pr[1]))
	}
	p.pairs = nil
	if err := write("uids", ub); err != nil {
		return indexMeta{}, err
	}

	terms := make([]string, 0, len(p.postings))
	for t := range p.postings {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	var postB []byte
	offs := make([]uint64, len(terms))
	lens := make([]uint64, len(terms))
	for i, t := range terms {
		offs[i] = uint64(len(postB))
		ps := p.postings[t]
		postB = binary.AppendUvarint(postB, uint64(len(ps)))
		prev := uint64(0)
		for _, q := range ps {
			d := q >> 16
			postB = binary.AppendUvarint(postB, d-prev)
			postB = binary.AppendUvarint(postB, q&0xffff)
			prev = d
		}
		lens[i] = uint64(len(postB)) - offs[i]
		delete(p.postings, t)
	}
	dictB, termIdx, termFilter := encodeTerms(terms, offs, lens)
	for name, b := range map[string][]byte{"terms": dictB, "terms.idx": termIdx, "terms.filter": termFilter, "postings": postB} {
		if err := write(name, b); err != nil {
			return indexMeta{}, err
		}
	}

	sort.Slice(p.faExact, func(i, j int) bool {
		if p.faExact[i].rel != p.faExact[j].rel {
			return p.faExact[i].rel < p.faExact[j].rel
		}
		return p.faExact[i].doc < p.faExact[j].doc
	})
	var fe []byte
	for _, e := range p.faExact {
		fe = appendStr(fe, e.rel)
		fe = binary.LittleEndian.AppendUint32(fe, e.doc)
	}
	var ft []byte
	for i, d := range p.faTio {
		ft = binary.LittleEndian.AppendUint32(ft, d)
		ft = binary.LittleEndian.AppendUint32(ft, p.faOffs[i])
	}
	ft = binary.LittleEndian.AppendUint32(ft, 0)
	ft = binary.LittleEndian.AppendUint32(ft, p.faOffs[len(p.faOffs)-1])
	tri, trip := p.tri.finish()
	for name, b := range map[string][]byte{"fa.exact": fe, "fa.tio": ft, "fa.tri": tri, "fa.trip": trip} {
		if err := write(name, b); err != nil {
			return indexMeta{}, err
		}
	}

	return indexMeta{Docs: p.nDocs, ScoredDocs: p.scoredDocs, TotalLen: p.totalLen, Harnesses: p.harnesses.list,
		SourceNames: p.sourceNames.list, Sessions: len(p.sessions), Turns: len(p.turns), Terms: len(terms)}, nil
}

// triBuilder maps every 3-byte sequence of each fa.blob entry to the ascending
// entries holding it: finish returns fa.tri, sorted (trigram u32, postings
// offset u32) pairs, and fa.trip, the uvarint count and entry gaps.
type triBuilder struct{ post map[uint32][]uint32 }

func newTriBuilder() triBuilder { return triBuilder{post: map[uint32][]uint32{}} }

func (t triBuilder) add(entry int, e string) {
	for j := 0; j+3 <= len(e); j++ {
		k := uint32(e[j])<<16 | uint32(e[j+1])<<8 | uint32(e[j+2])
		p := t.post[k]
		if n := len(p); n == 0 || p[n-1] != uint32(entry) {
			t.post[k] = append(p, uint32(entry))
		}
	}
}

func (t triBuilder) finish() ([]byte, []byte) {
	keys := make([]uint32, 0, len(t.post))
	for k := range t.post {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var tri, trip []byte
	for _, k := range keys {
		tri = binary.LittleEndian.AppendUint32(tri, k)
		tri = binary.LittleEndian.AppendUint32(tri, uint32(len(trip)))
		p := t.post[k]
		trip = binary.AppendUvarint(trip, uint64(len(p)))
		prev := uint32(0)
		for _, e := range p {
			trip = binary.AppendUvarint(trip, uint64(e-prev))
			prev = e
		}
	}
	return tri, trip
}

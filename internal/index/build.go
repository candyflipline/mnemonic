package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type built struct {
	row *Row
	src uint16
	lf  int64
	li  int
	// hrec is the row's heavy record, resolved from the holder part it was
	// read from; nil reads the record from the member's own heavy log at
	// place time (heavyReader.record).
	hrec []byte
}

// partEncoder and partHeavyEncoder compress a part's frames. A part is
// rewritten whole each time its member set changes (a settle, an append to a
// settled component), so its frames are on the cycle's path where a source
// log's are written once: they take the default level, not the source logs'
// better one.
var partEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithLowerEncoderMem(true))
var partHeavyEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithLowerEncoderMem(true))

// partFormat is hashed into every part ID, so a store built by an older layout
// rebuilds its parts instead of opening them.
const partFormat = 7

// fingerprint names a part by what its members parsed, never by how a disk
// laid the logs out, so a restored store names every part it did not change
// exactly as the store it came from does.
func fingerprint(cks []ckpt) string {
	h := sha256.New()
	fmt.Fprintf(h, "format %d\n", partFormat)
	for _, c := range cks {
		fmt.Fprintf(h, "%s %d %d %d %s %d\n", c.Key, c.Parser, c.Layout, c.ZstSize, c.WindowSHA, c.Rows)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type indexMeta struct {
	Fingerprint string   `json:"fingerprint"`
	Docs        int      `json:"docs"`
	ScoredDocs  uint64   `json:"scored_docs"`
	TotalLen    uint64   `json:"total_len"`
	Harnesses   []string `json:"harnesses"`
	SourceNames []string `json:"source_names"`
	Sessions    int      `json:"sessions"`
	Turns       int      `json:"turns"`
	Terms       int      `json:"terms"`
	Members     []member `json:"members,omitempty"`
}

// member is one source a part holds the rows of; a doc's src is its index in
// the part's members. Gen and the lengths name one disk's logs, so only a hot
// part carries them (deltaBase reads them); a cold part's bytes are the same
// on every disk that parsed the same raw.
type member struct {
	parsed
	Gen      int   `json:"gen,omitempty"`
	LightLen int64 `json:"light_len,omitempty"`
	HeavyLen int64 `json:"heavy_len,omitempty"`
}

func memberOf(c ckpt) member {
	return member{parsed: c.parsed, Gen: c.Gen, LightLen: c.LightLen, HeavyLen: c.HeavyLen}
}

func loadSourceRows(store string, si int, c ckpt, from int64) ([]built, error) {
	if from >= c.LightLen {
		return nil, nil
	}
	f, err := os.Open(logPath(store, c.Key, c.Gen, "light"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []built
	var scratch []byte
	for off := from; off < c.LightLen; {
		recs, next, err := readFrame(f, off, scratch)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			r := new(Row)
			if _, err := r.decode(rec); err != nil {
				return nil, err
			}
			r.Author = c.Author
			out = append(out, built{row: r, src: uint16(si)})
		}
		off = next
	}
	return out, nil
}

// holderIndexOf is the index of c's member entry in holder's members, -1 when
// holder does not hold it.
func holderIndexOf(holder *Part, key string) int {
	for i := range holder.meta.Members {
		if holder.meta.Members[i].Key == key {
			return i
		}
	}
	return -1
}

// memberFrom reports where c's tail starts when holder holds it: the light
// length the holder recorded, or, for a cold holder (a full part zeroes its
// members' lengths), no tail at all when the member provably has not parsed
// since — any resume grows Rows and any re-parse changes a parsed field or
// bumps the generation a hot holder records. ok is false when c must be read
// from its logs whole: the holder does not name it, or is left behind by a
// growth or re-parse it does not cover.
func memberFrom(c ckpt, holder *Part) (from int64, ok bool) {
	hs := holderIndexOf(holder, c.Key)
	if hs < 0 {
		return 0, false
	}
	hm := holder.meta.Members[hs]
	if hm.LightLen > 0 {
		if hm.Gen == c.Gen && hm.Rows <= c.Rows && hm.LightLen <= c.LightLen && hm.HeavyLen <= c.HeavyLen {
			return hm.LightLen, true
		}
		return 0, false
	}
	if sameParsed(hm.parsed, c.parsed) && hm.Rows == c.Rows {
		return c.LightLen, true
	}
	return 0, false
}

// sameParsed reports whether two checkpoints parsed the same bytes with the
// same parser and stopped in the same place, everything but the session list
// compared: a resume that adds rows changes ZstSize, State or Rows, and a
// re-parse changes one of those or the parser version.
func sameParsed(a, b parsed) bool {
	return a.Key == b.Key && a.Kind == b.Kind && a.Parser == b.Parser && a.Layout == b.Layout &&
		a.Harness == b.Harness && a.SourceName == b.SourceName && a.SourceFile == b.SourceFile &&
		a.Path == b.Path && a.Author == b.Author && a.Machine == b.Machine &&
		a.ZstSize == b.ZstSize && a.WindowSHA == b.WindowSHA && a.State == b.State && a.Rows == b.Rows
}

// memberRows returns one member's rows for a part build: the rows the holder
// part holds for it — the winners of the rows it covered, their heavy records
// resolved from the holder — plus the tail past the holder's copy, read from
// the member's frame logs. holder is the part of the previous manifest that
// holds the member, nil to read every row from the logs (a cold build's first
// run, or a holder a re-parse left behind); from is the tail start, memberFrom
// when holder is not nil. A member fully covered by its holder is read from
// the holder alone and its logs are never opened.
func memberRows(store string, si int, c ckpt, holder *Part, from int64) ([]built, error) {
	if holder == nil && c.retired() {
		return nil, fmt.Errorf("%s: retired member read without a holder", c.Key)
	}
	var out []built
	if holder != nil {
		hs := holderIndexOf(holder, c.Key)
		if hs < 0 {
			return nil, fmt.Errorf("%s: holder does not hold the member", c.Key)
		}
		for d := 0; d < holder.meta.Docs; d++ {
			rec := holder.docRec(d)
			if binary.LittleEndian.Uint16(rec[10:]) != uint16(hs) {
				continue
			}
			lf, li := int64(binary.LittleEndian.Uint64(rec[0:])), int(binary.LittleEndian.Uint16(rec[8:]))
			rows, _, err := readFrame(segments{holder.lightB, holder.lightD}, lf, nil)
			if err != nil {
				return nil, fmt.Errorf("%s: holder %s: %w", c.Key, holder.meta.Fingerprint, err)
			}
			if li >= len(rows) {
				return nil, fmt.Errorf("%s: holder %s: light index past frame", c.Key, holder.meta.Fingerprint)
			}
			r := new(Row)
			if _, err := r.decode(rows[li]); err != nil {
				return nil, fmt.Errorf("%s: holder %s: %w", c.Key, holder.meta.Fingerprint, err)
			}
			// The light record does not carry the author (strs skips it);
			// rows off the logs get it from the checkpoint, so do these.
			r.Author = c.Author
			hrecs, err := holder.fc.heavyFrame(holder, r.HeavyFrame)
			if err != nil {
				return nil, fmt.Errorf("%s: holder %s: %w", c.Key, holder.meta.Fingerprint, err)
			}
			if int(r.HeavyIdx) >= len(hrecs) {
				return nil, fmt.Errorf("%s: holder %s: heavy index past frame", c.Key, holder.meta.Fingerprint)
			}
			out = append(out, built{row: r, src: uint16(si), lf: lf, li: li, hrec: hrecs[r.HeavyIdx]})
		}
	}
	if from >= c.LightLen {
		return out, nil
	}
	tail, err := loadSourceRows(store, si, c, from)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.Key, err)
	}
	return append(out, tail...), nil
}

// openHolders opens, per member of the part being built, the part of the
// previous manifest that holds it: the one prev part naming the member's key,
// a delta opened as its image plus delta. A member no prev part names — a
// first build, or a member a regrouped plan moved — and one whose holder did
// not survive on disk gets nil: its rows come from its logs whole.
func openHolders(store string, cks []ckpt, prev manifestFile) (holders []*Part, close func(), err error) {
	byPrev := map[string]string{}
	scan := func(ids []string) {
		for _, id := range ids {
			d, ok := readBase(store, id)
			if !ok {
				continue
			}
			for _, m := range d.Members {
				if _, dup := byPrev[m.Key]; !dup {
					byPrev[m.Key] = id
				}
			}
		}
	}
	scan(prev.Hot)
	scan(prev.Parts)
	var opened []*Part
	holders = make([]*Part, len(cks))
	for i, c := range cks {
		id, ok := byPrev[c.Key]
		if !ok {
			continue
		}
		h, err := OpenPartDir(partDir(store, id))
		if err != nil {
			continue
		}
		opened = append(opened, h)
		if holderIndexOf(h, c.Key) >= 0 {
			holders[i] = h
		}
	}
	return holders, func() {
		for _, h := range opened {
			h.Close()
		}
	}, nil
}

// winner is a uid's winning row and the member it came from, kept per uid
// while a part's rows are offered: the row a full build picks for the uid.
type winner struct {
	b      built
	member member
}

// offerRow offers b, a row of member m, to win: within a member the higher
// version wins, last on a tie; across members memberBeats, the one order
// every read path (full build, delta image, holder rows) picks a uid's
// winner by. Two same-kind members of equal path hand the tie to the lesser
// key, the order buildIndex offers members in.
func offerRow(win map[string]winner, b built, m member) {
	if w, ok := win[b.row.UID]; ok {
		if w.b.src == b.src {
			if b.row.Version < w.b.row.Version {
				return
			}
		} else if !memberBeats(b.row.Version, m, w.b.row.Version, w.member) {
			return
		}
	}
	win[b.row.UID] = winner{b: b, member: m}
}

// mergeAuthors unions src's per-session authors into dst, each list sorted
// and distinct. A part's section is the union of its holders' sections and
// its tail rows': every row a holder covered, winner or not, is already in
// the holder's section, and unions do not double-count.
func mergeAuthors(dst, src map[string][]string) {
	for sid, as := range src {
		if len(as) == 0 {
			continue
		}
		merged := append([]string{}, dst[sid]...)
		merged = append(merged, as...)
		slices.Sort(merged)
		dst[sid] = slices.Compact(merged)
	}
}

func buildIndex(store string, cks []ckpt, fp, partDir string, prev manifestFile) error {
	sort.Slice(cks, func(i, j int) bool { return cks[i].Key < cks[j].Key })
	win := map[string]winner{}
	members := make([]member, len(cks))
	cold := !slices.ContainsFunc(cks, ckpt.hot)
	holders, closeHolders, err := openHolders(store, cks, prev)
	if err != nil {
		return err
	}
	defer closeHolders()
	authors := map[string][]string{}
	plan := planMembers(cks, holders)
	merged := map[*Part]bool{}
	for si, c := range cks {
		members[si] = memberOf(c)
		if cold {
			members[si].Gen, members[si].LightLen, members[si].HeavyLen = 0, 0, 0
		}
		holder := plan.holder[si]
		if holder != nil && !merged[holder] {
			merged[holder] = true
			mergeAuthors(authors, plan.holderAuthors(holder))
		}
		rows, err := memberRows(store, si, c, holder, plan.from[si])
		if err != nil {
			return err
		}
		for _, b := range rows {
			offerRow(win, b, members[si])
		}
		mergeAuthors(authors, authorsOf(rows, func(src uint16) string { return c.Author }))
	}
	bySession := map[string][]built{}
	for _, w := range win {
		bySession[w.b.row.Session] = append(bySession[w.b.row.Session], w.b)
	}
	win = nil

	heavy := newHeavyReader(store, cks)
	defer heavy.close()
	dir := partDir + ".tmp"
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lightF, err := openLog(filepath.Join(dir, "light"), 0)
	if err != nil {
		return err
	}
	defer lightF.Close()
	heavyF, err := openLog(filepath.Join(dir, "heavy"), 0)
	if err != nil {
		return err
	}
	defer heavyF.Close()
	lw := &frameWriter{f: lightF, target: lightFrame, enc: partEncoder}
	hw := &frameWriter{f: heavyF, target: heavyFrame, enc: partHeavyEncoder}
	place := func(b built) (*Row, int64, int, error) {
		hrec := b.hrec
		if hrec == nil {
			var err error
			if hrec, err = heavy.record(b); err != nil {
				return nil, 0, 0, err
			}
		}
		hf, hi, err := hw.add(hrec)
		if err != nil {
			return nil, 0, 0, err
		}
		own := *b.row
		own.HeavyFrame, own.HeavyIdx = uint64(hf), uint64(hi)
		lf, li, err := lw.add(own.append(nil))
		return &own, lf, li, err
	}
	write := func(name string, b []byte) error { return os.WriteFile(filepath.Join(dir, name), b, 0o644) }
	meta, lsRecs, err := indexRows(bySession, place, heavy.payload, write)
	if err != nil {
		return err
	}
	if heavy.err != nil {
		return heavy.err
	}
	if err := hw.flush(); err != nil {
		return err
	}
	if err := lw.flush(); err != nil {
		return err
	}
	if err := writeLs(dir, lsRecs); err != nil {
		return err
	}
	// The section build inside indexRows sees winner rows only; the session
	// authors section is a function of every row, winner or not.
	ab, err := json.Marshal(authors)
	if err != nil {
		return err
	}
	if err := write("authors", ab); err != nil {
		return err
	}
	meta.Fingerprint, meta.Members = fp, members
	mb, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o644); err != nil {
		return err
	}
	return commitPart(dir, partDir)
}

// indexRows writes the index files of the winning rows of one part through
// write. place puts a doc's row in the part's light log and returns the row as
// the part holds it; payload reads a row's payload_json for the ls rollups.
func indexRows(bySession map[string][]built, place func(built) (*Row, int64, int, error),
	payload func(built) string, write func(name string, b []byte) error) (indexMeta, []lsRec, error) {
	sessionIDs := make([]string, 0, len(bySession))
	for s := range bySession {
		sessionIDs = append(sessionIDs, s)
	}
	sort.Strings(sessionIDs)

	harnesses, sourceNames := dict{}, dict{}
	secs := newSections()
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
		lsRecs = append(lsRecs, lsAggregate(evs, turnSeqs[start:], payload))
		secs.addSession(sid, evs, turnSeqs[start:], payload)
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
	var hot, hotOff []byte
	for d, b := range docs {
		r, lf, li, err := place(b)
		if err != nil {
			return indexMeta{}, nil, err
		}
		hotOff = binary.LittleEndian.AppendUint64(hotOff, uint64(len(hot)))
		hot = hotRow(*r).append(hot)
		hotOff = binary.LittleEndian.AppendUint64(hotOff, textHash(r.Text))
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
		binary.LittleEndian.PutUint64(rec[0:], uint64(lf))
		binary.LittleEndian.PutUint16(rec[8:], uint16(li))
		binary.LittleEndian.PutUint16(rec[10:], b.src)
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
	hotOff = binary.LittleEndian.AppendUint64(hotOff, uint64(len(hot)))
	for name, b := range map[string][]byte{"hot": hot, "hot.off": hotOff, "docs": docCol} {
		if err := write(name, b); err != nil {
			return indexMeta{}, nil, err
		}
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
	for name, b := range map[string][]byte{"sessions": sb, "sessions.str": sstr, "turns": tb} {
		if err := write(name, b); err != nil {
			return indexMeta{}, nil, err
		}
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
	if err := write("uids", ub); err != nil {
		return indexMeta{}, nil, err
	}

	terms := make([]string, 0, len(postings))
	for t := range postings {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	var postB []byte
	offs := make([]uint64, len(terms))
	lens := make([]uint64, len(terms))
	for i, t := range terms {
		offs[i] = uint64(len(postB))
		ps := postings[t]
		postB = binary.AppendUvarint(postB, uint64(len(ps)))
		prev := uint64(0)
		for _, p := range ps {
			d := p >> 16
			postB = binary.AppendUvarint(postB, d-prev)
			postB = binary.AppendUvarint(postB, p&0xffff)
			prev = d
		}
		lens[i] = uint64(len(postB)) - offs[i]
		delete(postings, t)
	}
	dictB, termIdx, termFilter := encodeTerms(terms, offs, lens)
	for name, b := range map[string][]byte{"terms": dictB, "terms.idx": termIdx, "terms.filter": termFilter, "postings": postB} {
		if err := write(name, b); err != nil {
			return indexMeta{}, nil, err
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
	tri, trip := trigramIndex(faBlob, faOffs)
	for name, b := range map[string][]byte{"fa.exact": fe, "fa.tio": ft, "fa.blob": faBlob, "fa.tri": tri, "fa.trip": trip} {
		if err := write(name, b); err != nil {
			return indexMeta{}, nil, err
		}
	}

	for name, b := range secs.files() {
		if err := write(name, b); err != nil {
			return indexMeta{}, nil, err
		}
	}

	return indexMeta{Docs: n, ScoredDocs: scoredDocs, TotalLen: totalLen, Harnesses: harnesses.list,
		SourceNames: sourceNames.list, Sessions: len(sessions), Turns: len(turns), Terms: len(terms)}, lsRecs, nil
}

type faEntry struct {
	rel string
	doc uint32
}

const docWidth = 40

// commitPart fsyncs every file of the part built in tmp, then renames it to
// dir. A part dir that exists is complete; the manifest names it after.
func commitPart(tmp, dir string) error {
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range ents {
		f, err := os.Open(filepath.Join(tmp, e.Name()))
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := syncDir(tmp); err != nil {
		return err
	}
	return renameSynced(tmp, dir)
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

// heavyReader reads the heavy records of rows at build, decoding a few frames
// at a time; a row's src picks its log. Logs open on first use: a member whose
// rows its holder carries never opens its own.
type heavyReader struct {
	store string
	cks   []ckpt
	logs  []io.ReaderAt
	files []*os.File
	recs  map[heavyKey][][]byte
	order []heavyKey
	err   error
}

type heavyKey struct {
	src   uint16
	frame uint64
}

func newHeavyReader(store string, cks []ckpt) *heavyReader {
	return &heavyReader{store: store, cks: cks, recs: map[heavyKey][][]byte{}}
}

// logAt is the heavy log of member src, opened on first use.
func (h *heavyReader) logAt(src int) (io.ReaderAt, error) {
	for len(h.logs) <= src {
		h.logs = append(h.logs, nil)
	}
	if h.logs[src] != nil {
		return h.logs[src], nil
	}
	f, err := os.Open(logPath(h.store, h.cks[src].Key, h.cks[src].Gen, "heavy"))
	if err != nil {
		h.err = err
		return nil, err
	}
	h.files = append(h.files, f)
	h.logs[src] = f
	return f, nil
}

// record is the heavy record of b: payload_json then token_usage_json, each a
// uvarint length and its bytes.
func (h *heavyReader) record(b built) ([]byte, error) {
	if b.hrec != nil {
		return b.hrec, nil
	}
	if h.err != nil {
		return nil, h.err
	}
	k := heavyKey{src: b.src, frame: b.row.HeavyFrame}
	recs, ok := h.recs[k]
	if !ok {
		log, err := h.logAt(int(k.src))
		if err != nil {
			return nil, err
		}
		recs, _, err = readFrame(log, int64(k.frame), nil)
		if err != nil {
			h.err = err
			return nil, err
		}
		h.recs[k] = recs
		h.order = append(h.order, k)
		if len(h.order) > 16 {
			delete(h.recs, h.order[0])
			h.order = h.order[1:]
		}
	}
	if int(b.row.HeavyIdx) >= len(recs) {
		h.err = fmt.Errorf("heavy index past frame")
		return nil, h.err
	}
	return recs[b.row.HeavyIdx], nil
}

// heavyPayload is a heavy record's payload_json.
func heavyPayload(rec []byte) string {
	l, n := binary.Uvarint(rec)
	return string(rec[n : n+int(l)])
}

func (h *heavyReader) payload(b built) string {
	rec, err := h.record(b)
	if err != nil {
		return ""
	}
	return heavyPayload(rec)
}

func (h *heavyReader) close() {
	for _, f := range h.files {
		if f != nil {
			f.Close()
		}
	}
}

// hotRow is the part of r search and file_attention hydrate, kept
// uncompressed in the index so a hit reads one record and no log frame:
// everything but text_content for tool requests, and only the hit fields
// for every other row.
func hotRow(r Row) *Row {
	r.Text = ""
	if r.ToolPhase != "request" {
		r.SourceFile, r.ItemID, r.ToolCallID, r.EndpointKind = "", "", "", ""
		r.ProjectID, r.RepoRelPath, r.WorktreeRoot, r.Cwd = "", "", "", ""
		r.Buckets, r.Native = "", ""
		r.TioInput, r.TioInPrev, r.TioOutPrev, r.TioProj, r.TioRel, r.TioWT = "", "", "", "", "", ""
	}
	return &r
}

// trigramIndex maps every 3-byte sequence of each fa.blob entry to the
// ascending entries holding it: fa.tri is sorted (trigram u32, postings
// offset u32) pairs, fa.trip the uvarint count and entry gaps.
func trigramIndex(blob []byte, offs []uint32) ([]byte, []byte) {
	post := map[uint32][]uint32{}
	for i := 0; i+1 < len(offs); i++ {
		e := blob[offs[i]:offs[i+1]]
		for j := 0; j+3 <= len(e); j++ {
			k := trigram(e[j:])
			p := post[k]
			if n := len(p); n == 0 || p[n-1] != uint32(i) {
				post[k] = append(p, uint32(i))
			}
		}
	}
	keys := make([]uint32, 0, len(post))
	for k := range post {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var tri, trip []byte
	for _, k := range keys {
		tri = binary.LittleEndian.AppendUint32(tri, k)
		tri = binary.LittleEndian.AppendUint32(tri, uint32(len(trip)))
		p := post[k]
		trip = binary.AppendUvarint(trip, uint64(len(p)))
		prev := uint32(0)
		for _, e := range p {
			trip = binary.AppendUvarint(trip, uint64(e-prev))
			prev = e
		}
	}
	return tri, trip
}

func trigram(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

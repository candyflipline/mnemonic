package index

import (
	"bufio"
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

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/klauspost/compress/zstd"
)

type built struct {
	row *Row
	src uint16
	lf  int64
	li  int
}

func (b built) after(w built) bool {
	return b.lf > w.lf || b.lf == w.lf && b.li > w.li
}

var partEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithLowerEncoderMem(true))
var partHeavyEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)), zstd.WithLowerEncoderMem(true))

// partFormat is hashed into every part ID, so a store built by an older layout
// rebuilds its parts instead of opening them.
const partFormat = 7

// fingerprint reads only what members parsed, so a restored store names every
// part it did not change exactly as the store it came from does.
func fingerprint(cks []ckpt) string {
	h := sha256.New()
	fmt.Fprintf(h, "format %d\n", partFormat)
	for _, c := range cks {
		p := c.parsed
		fmt.Fprintf(h, "%s %d %d %d %s %d\n", p.Key, p.Parser, p.Layout, p.Offset, p.WindowSHA, p.Rows)
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
// the part's members.
type member struct {
	parsed
	hotPos
}

type hotPos struct {
	Gen      int   `json:"gen,omitempty"`
	LightLen int64 `json:"light_len,omitempty"`
	HeavyLen int64 `json:"heavy_len,omitempty"`
}

func memberOf(c ckpt) member {
	return member{parsed: c.parsed, hotPos: hotPos{Gen: c.Gen, LightLen: c.LightLen, HeavyLen: c.HeavyLen}}
}

type rowLoc struct {
	lf     int64
	li     uint16
	holder bool
}

func (l rowLoc) after(w rowLoc) bool {
	switch {
	case l.holder != w.holder:
		return w.holder
	case l.lf != w.lf:
		return l.lf > w.lf
	}
	return l.li > w.li
}

func walkLog(store string, c ckpt, from int64, fn func(rec []byte, off int64, i int) error) error {
	if from >= c.LightLen {
		return nil
	}
	f, err := os.Open(logPath(store, c.Key, c.Gen, "light"))
	if err != nil {
		return err
	}
	defer f.Close()
	var scratch []byte
	for off := from; off < c.LightLen; {
		recs, next, err := readFrame(f, off, scratch)
		if err != nil {
			return err
		}
		for i, rec := range recs {
			if err := fn(rec, off, i); err != nil {
				return err
			}
		}
		off = next
	}
	return nil
}

func (c *ckpt) decodeRow(r *Row, rec []byte) error {
	if _, err := r.decode(rec); err != nil {
		return err
	}
	r.Author = c.Author
	return nil
}

func loadSourceRows(store string, si int, c ckpt, from int64) ([]built, error) {
	var out []built
	err := walkLog(store, c, from, func(rec []byte, _ int64, _ int) error {
		r := new(Row)
		if err := c.decodeRow(r, rec); err != nil {
			return err
		}
		out = append(out, built{row: r, src: uint16(si)})
		return nil
	})
	return out, err
}

func holderIndexOf(holder *Part, key string) int {
	for i := range holder.meta.Members {
		if holder.meta.Members[i].Key == key {
			return i
		}
	}
	return -1
}

// memberFrom reports where c's tail starts when holder holds it. ok is false
// when c must be read from its logs whole.
func memberFrom(c ckpt, holder *Part) (from int64, ok bool) {
	hs := holderIndexOf(holder, c.Key)
	if hs < 0 {
		return 0, false
	}
	hm := holder.meta.Members[hs]
	switch {
	case hm.LightLen > 0:
		if hotHolderIsPrefix(hm, c) {
			return hm.LightLen, true
		}
	case coldHolderUnchanged(hm, c):
		return c.LightLen, true
	case coldHolderResumed(hm, c):
		return c.ColdLight, true
	}
	return 0, false
}

func hotHolderIsPrefix(hm member, c ckpt) bool {
	return hm.Gen == c.Gen && hm.Rows <= c.Rows && hm.LightLen <= c.LightLen && hm.HeavyLen <= c.HeavyLen
}

func coldHolderUnchanged(hm member, c ckpt) bool {
	return sameParsed(hm.parsed, c.parsed) && hm.Rows == c.Rows
}

func coldHolderResumed(hm member, c ckpt) bool {
	if c.ColdLight == 0 {
		return false
	}
	atColdSettle := c.parsed
	atColdSettle.Offset, atColdSettle.Rows, atColdSettle.WindowSHA, atColdSettle.State = c.ColdZst, c.ColdRows, c.ColdSHA, hm.State
	return sameParsed(hm.parsed, atColdSettle) && c.Rows > c.ColdRows && c.LightLen >= c.ColdLight
}

func sameParsed(a, b parsed) bool {
	return a.Key == b.Key && a.Kind == b.Kind && a.Parser == b.Parser && a.Layout == b.Layout &&
		a.Harness == b.Harness && a.SourceName == b.SourceName && a.SourceFile == b.SourceFile &&
		a.Path == b.Path && a.Author == b.Author && a.Machine == b.Machine &&
		a.Offset == b.Offset && a.WindowSHA == b.WindowSHA && a.State == b.State && a.Rows == b.Rows
}

// walkMember calls fn with the light record of every row a part build reads
// of one member: the rows the holder part holds for it, then the tail from
// from, read from the member's frame logs. holder is the part of the previous
// manifest that holds the member, nil to read every row from the logs (a cold
// build's first run, or a holder a re-parse left behind). A member fully
// covered by its holder is read from the holder alone and its logs are never
// opened. rec is valid until fn returns.
func walkMember(store string, c ckpt, holder *Part, from int64, fn func(rec []byte, loc rowLoc) error) error {
	if holder == nil && c.retired() {
		return fmt.Errorf("%s: retired member read without a holder", c.Key)
	}
	if holder != nil {
		hs := holderIndexOf(holder, c.Key)
		if hs < 0 {
			return fmt.Errorf("%s: holder does not hold the member", c.Key)
		}
		var frameLF int64 = -1
		var frameRows [][]byte
		for d := 0; d < holder.meta.Docs; d++ {
			rec := holder.docRec(d)
			if binary.LittleEndian.Uint16(rec[10:]) != uint16(hs) {
				continue
			}
			lf, li := int64(binary.LittleEndian.Uint64(rec[0:])), int(binary.LittleEndian.Uint16(rec[8:]))
			if lf != frameLF {
				rows, _, err := readFrame(segments{holder.lightB, holder.lightD}, lf, nil)
				if err != nil {
					return fmt.Errorf("%s: holder %s: %w", c.Key, holder.meta.Fingerprint, err)
				}
				frameLF, frameRows = lf, rows
			}
			if li >= len(frameRows) {
				return fmt.Errorf("%s: holder %s: light index past frame", c.Key, holder.meta.Fingerprint)
			}
			if err := fn(frameRows[li], rowLoc{lf: lf, li: uint16(li), holder: true}); err != nil {
				return err
			}
		}
	}
	err := walkLog(store, c, from, func(rec []byte, off int64, i int) error {
		return fn(rec, rowLoc{lf: off, li: uint16(i)})
	})
	if err != nil {
		return fmt.Errorf("%s: %w", c.Key, err)
	}
	return nil
}

func memberRows(store string, si int, c ckpt, holder *Part, from int64) ([]built, error) {
	var out []built
	err := walkMember(store, c, holder, from, func(rec []byte, loc rowLoc) error {
		r := new(Row)
		if err := c.decodeRow(r, rec); err != nil {
			if loc.holder {
				return fmt.Errorf("%s: holder %s: %w", c.Key, holder.meta.Fingerprint, err)
			}
			return err
		}
		b := built{row: r, src: uint16(si)}
		if loc.holder {
			b.lf, b.li = loc.lf, int(loc.li)
		}
		out = append(out, b)
		return nil
	})
	return out, err
}

func prevPartsMap(store string, prev manifestFile) map[string]string {
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
	return byPrev
}

func openHolders(store string, cks []ckpt, byPrev map[string]string) (holders []*Part, close func(), err error) {
	var opened []*Part
	byID := map[string]*Part{}
	holders = make([]*Part, len(cks))
	for i, c := range cks {
		id, ok := byPrev[c.Key]
		if !ok {
			continue
		}
		h, seen := byID[id]
		if !seen {
			if p, err := OpenPartDir(partDir(store, id)); err == nil {
				h = p
				opened = append(opened, p)
			}
			byID[id] = h
		}
		if h == nil {
			continue
		}
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

type winner struct {
	b      built
	member member
}

func offerRow(win map[string]winner, b built, m member) {
	if w, ok := win[b.row.UID]; ok {
		if w.b.src == b.src {
			if b.row.Version < w.b.row.Version || b.row.Version == w.b.row.Version && !b.after(w.b) {
				return
			}
		} else if !memberBeats(b.row.Version, m, w.b.row.Version, w.member) {
			return
		}
	}
	win[b.row.UID] = winner{b: b, member: m}
}

func collectAnnotations(store string, c ckpt, src uint16, heavy *heavyReader, into map[string][]annRow) error {
	return walkLog(store, c, 0, func(rec []byte, _ int64, _ int) error {
		r := new(Row)
		if err := c.decodeRow(r, rec); err != nil {
			return err
		}
		hrec, err := heavy.record(src, r)
		if err != nil {
			return err
		}
		into[r.Session] = append(into[r.Session], annRow{payload: heavyPayload(hrec), author: c.Author})
		return nil
	})
}

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

func buildIndex(store string, cks []ckpt, fp, partDir string, byPrev map[string]string) error {
	sort.Slice(cks, func(i, j int) bool { return cks[i].Key < cks[j].Key })
	members := make([]member, len(cks))
	cold := !slices.ContainsFunc(cks, ckpt.hot)
	holders, closeHolders, err := openHolders(store, cks, byPrev)
	if err != nil {
		return err
	}
	defer closeHolders()
	allRowAuthors := map[string][]string{}
	plan := planMembers(cks, holders)
	merged := map[*Part]bool{}
	rows := 0
	for _, c := range cks {
		rows += int(c.Rows)
	}
	kp := newKeyPass(members, rows)
	heavy := newHeavyReader(store, cks)
	defer heavy.close()
	annRows := map[string][]annRow{}
	for si, c := range cks {
		members[si] = memberOf(c)
		if cold {
			members[si].hotPos = hotPos{}
		}
		if c.Harness == schema.AnnotationHarness {
			if err := collectAnnotations(store, c, uint16(si), heavy, annRows); err != nil {
				return err
			}
			continue
		}
		holder := plan.holder[si]
		if holder != nil && !merged[holder] {
			merged[holder] = true
			mergeAuthors(allRowAuthors, plan.holderAuthors(holder))
		}
		err := walkMember(store, c, holder, plan.from[si], func(rec []byte, loc rowLoc) error {
			return kp.offer(uint16(si), c.Author, rec, loc)
		})
		if err != nil {
			return err
		}
	}
	mergeAuthors(allRowAuthors, kp.authorsBySession(cks))
	locs := kp.takeWinners()
	sessionIDs := kp.sessions.list
	kp = nil

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
	hotF, err := os.Create(filepath.Join(dir, "hot"))
	if err != nil {
		return err
	}
	defer hotF.Close()
	hotOffF, err := os.Create(filepath.Join(dir, "hot.off"))
	if err != nil {
		return err
	}
	defer hotOffF.Close()
	docsF, err := os.Create(filepath.Join(dir, "docs"))
	if err != nil {
		return err
	}
	defer docsF.Close()
	blobF, err := os.Create(filepath.Join(dir, "fa.blob"))
	if err != nil {
		return err
	}
	defer blobF.Close()
	buffered := func(f *os.File) *bufio.Writer { return bufio.NewWriterSize(f, 1<<18) }
	hot, hotOff, docs, blob := buffered(hotF), buffered(hotOffF), buffered(docsF), buffered(blobF)
	lw := &frameWriter{f: lightF, target: lightFrame, enc: partEncoder}
	hw := &frameWriter{f: heavyF, target: heavyFrame, enc: partHeavyEncoder}

	acc := newPartAcc(len(locs), hot, hotOff, docs, blob)
	lights := &lightSource{store: store, cks: cks, holders: plan.holder, frames: newFrameLRU(lightFrames)}
	var r Row
	var recBuf []byte
	var feed *sessionFeed
	curSession := ^uint32(0)
	for i := range locs {
		k := locs[i]
		loc := k.rowLoc()
		rec, err := lights.record(k.src, loc)
		if err != nil {
			return err
		}
		r = Row{}
		if err := cks[k.src].decodeRow(&r, rec); err != nil {
			return fmt.Errorf("%s: %w", cks[k.src].Key, err)
		}
		var hrec []byte
		if loc.holder {
			h := plan.holder[k.src]
			hrecs, err := h.fc.heavyFrame(h, r.HeavyFrame)
			if err != nil {
				return fmt.Errorf("%s: holder %s: %w", cks[k.src].Key, h.meta.Fingerprint, err)
			}
			if int(r.HeavyIdx) >= len(hrecs) {
				return fmt.Errorf("%s: holder %s: heavy index past frame", cks[k.src].Key, h.meta.Fingerprint)
			}
			hrec = hrecs[r.HeavyIdx]
		} else if hrec, err = heavy.record(k.src, &r); err != nil {
			return err
		}
		hf, hi, err := hw.add(hrec)
		if err != nil {
			return err
		}
		r.HeavyFrame, r.HeavyIdx = uint64(hf), uint64(hi)
		recBuf = r.append(recBuf[:0])
		lf, li, err := lw.add(recBuf)
		if err != nil {
			return err
		}
		if k.session != curSession {
			feed, curSession = acc.session(sessionIDs[k.session]), k.session
		}
		if _, err := feed.add(&r, k.src, lf, li); err != nil {
			return err
		}
	}
	locs = nil
	if heavy.err != nil {
		return heavy.err
	}
	if err := hw.flush(); err != nil {
		return err
	}
	if err := lw.flush(); err != nil {
		return err
	}
	if err := acc.sealDocs(); err != nil {
		return err
	}

	secs := newSections()
	var lsRecs []lsRec
	view := &partView{acc: acc, cks: cks, docs: docsF, light: lightF,
		frames: newFrameLRU(sectionFrames), heavy: newPartHeavy(heavyF)}
	for si := range acc.sessions {
		view.begin(si)
		lsRecs = append(lsRecs, lsAggregate(view.withSeqs(), view.payload))
		secs.addSession(acc.sessionIDs[si], view.rows(), view.payload)
		if view.err != nil {
			return view.err
		}
	}
	if view.heavy.err != nil {
		return view.heavy.err
	}
	for sid, rows := range annRows {
		secs.ann[sid] = buildAnnotations(rows)
	}

	write := func(name string, b []byte) error { return os.WriteFile(filepath.Join(dir, name), b, 0o644) }
	meta, err := acc.finish(write)
	if err != nil {
		return err
	}
	if err := secs.write(allRowAuthors, write); err != nil {
		return err
	}
	if err := writeLs(dir, lsRecs); err != nil {
		return err
	}
	meta.Fingerprint, meta.Members = fp, members
	mb, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o644); err != nil {
		return err
	}
	return commitPart(dir, partDir)
}

func indexRows(bySession map[string][]built, allRowAuthors map[string][]string, place func(built) (*Row, int64, int, error),
	payload func(built) string, write func(name string, b []byte) error) (indexMeta, []lsRec, error) {
	sessionIDs := make([]string, 0, len(bySession))
	for s := range bySession {
		sessionIDs = append(sessionIDs, s)
	}
	sort.Strings(sessionIDs)

	n := 0
	for _, evs := range bySession {
		n += len(evs)
	}
	mem := func(name string) *memFile { return &memFile{name: name, write: write} }
	acc := newPartAcc(n, mem("hot"), mem("hot.off"), mem("docs"), mem("fa.blob"))
	secs := newSections()
	var lsRecs []lsRec
	for _, sid := range sessionIDs {
		evs := bySession[sid]
		sort.Slice(evs, func(i, j int) bool { return orderLess(evs[i].row, evs[j].row) })
		feed := acc.session(sid)
		seqs := make([]uint32, len(evs))
		for k, b := range evs {
			r, lf, li, err := place(b)
			if err != nil {
				return indexMeta{}, nil, err
			}
			if seqs[k], err = feed.add(r, b.src, lf, li); err != nil {
				return indexMeta{}, nil, err
			}
		}
		view := &sessionView{evs: evs, seqs: seqs, payloadOf: payload}
		lsRecs = append(lsRecs, lsAggregate(view.withSeqs(), view.payload))
		secs.addSession(sid, view.rows(), view.payload)
		delete(bySession, sid)
	}
	meta, err := acc.finish(write)
	if err != nil {
		return indexMeta{}, nil, err
	}
	if err := secs.write(allRowAuthors, write); err != nil {
		return indexMeta{}, nil, err
	}
	return meta, lsRecs, nil
}

type faEntry struct {
	rel string
	doc uint32
}

const docWidth = 40

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

func (h *heavyReader) record(src uint16, r *Row) ([]byte, error) {
	if h.err != nil {
		return nil, h.err
	}
	k := heavyKey{src: src, frame: r.HeavyFrame}
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
	if int(r.HeavyIdx) >= len(recs) {
		h.err = fmt.Errorf("heavy index past frame")
		return nil, h.err
	}
	return recs[r.HeavyIdx], nil
}

func heavyPayload(rec []byte) string {
	l, n := binary.Uvarint(rec)
	return string(rec[n : n+int(l)])
}

func newPartHeavy(log io.ReaderAt) *heavyReader {
	return &heavyReader{logs: []io.ReaderAt{log}, recs: map[heavyKey][][]byte{}}
}

func (h *heavyReader) partPayload(r *Row) string {
	rec, err := h.record(0, r)
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

func trigram(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

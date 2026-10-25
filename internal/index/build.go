package index

import (
	"bufio"
	"bytes"
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

// rowLoc says where a row's light record is: the member's light log at frame
// lf, or, for a row its holder part supplies, the holder part's light log.
type rowLoc struct {
	lf     int64
	li     uint16
	holder bool
}

// walkLog calls fn with every light record of c's log from the frame at from,
// with the record's frame offset and index in the frame.
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

func loadSourceRows(store string, si int, c ckpt, from int64) ([]built, error) {
	var out []built
	err := walkLog(store, c, from, func(rec []byte, _ int64, _ int) error {
		r := new(Row)
		if _, err := r.decode(rec); err != nil {
			return err
		}
		r.Author = c.Author
		out = append(out, built{row: r, src: uint16(si)})
		return nil
	})
	return out, err
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
// bumps the generation a hot holder records — or the tail past the length the
// checkpoint recorded at the settle the cold holder was built from: a resume
// keeps the generation and a re-parse clears those fields. ok is false when c must be read
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
	if c.ColdLight > 0 {
		at := c.parsed
		at.ZstSize, at.Rows, at.WindowSHA, at.State = c.ColdZst, c.ColdRows, c.ColdSHA, hm.State
		if sameParsed(hm.parsed, at) && c.Rows > c.ColdRows && c.LightLen >= c.ColdLight {
			return c.ColdLight, true
		}
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

// walkMember calls fn with the light record of every row a part build reads
// of one member, in the order the rows are offered: the rows the holder part
// holds for it, then the tail past the holder's copy, read from the member's
// frame logs. holder is the part of the previous manifest that holds the
// member, nil to read every row from the logs (a cold build's first run, or a
// holder a re-parse left behind); from is the tail start, memberFrom when
// holder is not nil. A member fully covered by its holder is read from the
// holder alone and its logs are never opened. rec is valid until fn returns.
func walkMember(store string, c ckpt, holder *Part, from int64, fn func(rec []byte, loc rowLoc) error) error {
	if holder == nil && c.retired() {
		return fmt.Errorf("%s: retired member read without a holder", c.Key)
	}
	if holder != nil {
		hs := holderIndexOf(holder, c.Key)
		if hs < 0 {
			return fmt.Errorf("%s: holder does not hold the member", c.Key)
		}
		// The light frame the previous doc read: a frame's docs are
		// contiguous in the part's docs, so one member read decodes each
		// of its frames once, not once per row.
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

// memberRows returns one member's rows for a delta image: walkMember's
// records decoded, the holder's with their heavy records resolved from it.
func memberRows(store string, si int, c ckpt, holder *Part, from int64) ([]built, error) {
	var out []built
	err := walkMember(store, c, holder, from, func(rec []byte, loc rowLoc) error {
		r := new(Row)
		if _, err := r.decode(rec); err != nil {
			if loc.holder {
				return fmt.Errorf("%s: holder %s: %w", c.Key, holder.meta.Fingerprint, err)
			}
			return err
		}
		// The light record does not carry the author (strs skips it);
		// rows off the logs get it from the checkpoint, so do these.
		r.Author = c.Author
		b := built{row: r, src: uint16(si)}
		if loc.holder {
			hrecs, err := holder.fc.heavyFrame(holder, r.HeavyFrame)
			if err != nil {
				return fmt.Errorf("%s: holder %s: %w", c.Key, holder.meta.Fingerprint, err)
			}
			if int(r.HeavyIdx) >= len(hrecs) {
				return fmt.Errorf("%s: holder %s: heavy index past frame", c.Key, holder.meta.Fingerprint)
			}
			b.lf, b.li, b.hrec = loc.lf, int(loc.li), hrecs[r.HeavyIdx]
		}
		out = append(out, b)
		return nil
	})
	return out, err
}

// prevPartsMap is the previous manifest's key -> part id map, the one scan of
// prev a cycle pays: hot parts first, then cold, first id wins per key. It is
// built once per cycle, after readManifest, and handed to rematerialize and
// down the build path, so the manifest is not re-scanned per part built.
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

// openHolders opens, per member of the part being built, the part of the
// previous manifest that holds it: the one prev part naming the member's key,
// a delta opened as its image plus delta. byPrev is the cycle's precomputed
// prevPartsMap. A member no prev part names — a first build, or a member a
// regrouped plan moved — and one whose holder did not survive on disk gets
// nil: its rows come from its logs whole.
func openHolders(store string, cks []ckpt, byPrev map[string]string) (holders []*Part, close func(), err error) {
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

func buildIndex(store string, cks []ckpt, fp, partDir string, byPrev map[string]string) error {
	sort.Slice(cks, func(i, j int) bool { return cks[i].Key < cks[j].Key })
	members := make([]member, len(cks))
	cold := !slices.ContainsFunc(cks, ckpt.hot)
	holders, closeHolders, err := openHolders(store, cks, byPrev)
	if err != nil {
		return err
	}
	defer closeHolders()
	authors := map[string][]string{}
	plan := planMembers(cks, holders)
	merged := map[*Part]bool{}
	rows := 0
	for _, c := range cks {
		rows += int(c.Rows)
	}
	kp := newKeyPass(members, rows)
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
		err := walkMember(store, c, holder, plan.from[si], func(rec []byte, loc rowLoc) error {
			return kp.offer(uint16(si), c.Author, rec, loc)
		})
		if err != nil {
			return err
		}
	}
	mergeAuthors(authors, kp.authorsBySession(cks))
	locs := kp.sorted()
	sessionIDs := kp.sessions.list
	kp = nil

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

	// Doc pass: each winner's light record is decoded once, in doc order.
	acc := newPartAcc(len(locs), hot, hotOff, docs, blob)
	lights := &lightSource{store: store, cks: cks, holders: plan.holder, frames: newFrameLRU(lightFrames)}
	var r Row
	var recBuf []byte
	curSession := ^uint32(0)
	for i := range locs {
		k := locs[i]
		loc := k.rowLoc()
		rec, err := lights.record(k.src, loc)
		if err != nil {
			return err
		}
		r = Row{}
		if _, err := r.decode(rec); err != nil {
			return fmt.Errorf("%s: %w", cks[k.src].Key, err)
		}
		r.Author = cks[k.src].Author
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
		} else if hrec, err = heavy.record(built{row: &r, src: k.src}); err != nil {
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
			if curSession != ^uint32(0) {
				acc.endSession()
			}
			acc.beginSession(sessionIDs[k.session])
			curSession = k.session
		}
		if _, err := acc.add(&r, k.src, lf, li); err != nil {
			return err
		}
	}
	if curSession != ^uint32(0) {
		acc.endSession()
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
	if err := docs.Flush(); err != nil {
		return err
	}

	// Section pass: the derived sections read each session's winners back
	// from the part's own logs.
	secs := newSections()
	var lsRecs []lsRec
	view := &partView{acc: acc, cks: cks, docs: docsF, light: lightF,
		frames: newFrameLRU(sectionFrames), heavy: &heavyReader{logs: []io.ReaderAt{heavyF}, recs: map[heavyKey][][]byte{}}}
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

	write := func(name string, b []byte) error { return os.WriteFile(filepath.Join(dir, name), b, 0o644) }
	meta, err := acc.finish(write)
	if err != nil {
		return err
	}
	for _, w := range []*bufio.Writer{hot, hotOff, blob} {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	for name, b := range secs.files() {
		if name == "authors" {
			continue
		}
		if err := write(name, b); err != nil {
			return err
		}
	}
	if err := writeLs(dir, lsRecs); err != nil {
		return err
	}
	// The section build sees winner rows only; the session authors section
	// is a function of every row, winner or not.
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

// indexRows writes the index files of the winning rows of one part held in
// memory, the delta image's, through write. place puts a doc's row in the
// part's light log and returns the row as the part holds it; payload reads a
// row's payload_json for the ls rollups.
func indexRows(bySession map[string][]built, place func(built) (*Row, int64, int, error),
	payload func(built) string, write func(name string, b []byte) error) (indexMeta, []lsRec, error) {
	sessionIDs := make([]string, 0, len(bySession))
	for s := range bySession {
		sessionIDs = append(sessionIDs, s)
	}
	sort.Strings(sessionIDs)

	var hot, hotOff, docs, blob bytes.Buffer
	n := 0
	for _, evs := range bySession {
		n += len(evs)
	}
	acc := newPartAcc(n, &hot, &hotOff, &docs, &blob)
	secs := newSections()
	var lsRecs []lsRec
	for _, sid := range sessionIDs {
		evs := bySession[sid]
		sort.Slice(evs, func(i, j int) bool { return orderLess(evs[i].row, evs[j].row) })
		acc.beginSession(sid)
		seqs := make([]uint32, len(evs))
		for k, b := range evs {
			r, lf, li, err := place(b)
			if err != nil {
				return indexMeta{}, nil, err
			}
			if seqs[k], err = acc.add(r, b.src, lf, li); err != nil {
				return indexMeta{}, nil, err
			}
		}
		acc.endSession()
		view := &sessionView{evs: evs, seqs: seqs, payloadOf: payload}
		lsRecs = append(lsRecs, lsAggregate(view.withSeqs(), view.payload))
		secs.addSession(sid, view.rows(), view.payload)
		delete(bySession, sid)
	}
	for name, b := range map[string][]byte{"hot": hot.Bytes(), "hot.off": hotOff.Bytes(), "docs": docs.Bytes(), "fa.blob": blob.Bytes()} {
		if err := write(name, b); err != nil {
			return indexMeta{}, nil, err
		}
	}
	meta, err := acc.finish(write)
	if err != nil {
		return indexMeta{}, nil, err
	}
	for name, b := range secs.files() {
		if err := write(name, b); err != nil {
			return indexMeta{}, nil, err
		}
	}
	return meta, lsRecs, nil
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

func trigram(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

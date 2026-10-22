package trace

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
)

// A hot component is held as an image, a full part built at an earlier
// append, plus a delta: the rows appended since, re-framed into append-only
// logs under deltas/<image id>. Its part dir holds only deltaFile.
const deltaFile = "delta.json"

const foldRatio = 1

type delta struct {
	Image    string   `json:"image"`
	LightLen int64    `json:"light_len"`
	HeavyLen int64    `json:"heavy_len"`
	Rows     int64    `json:"rows"`
	Members  []member `json:"members"`
	listed
}

// listed is what ListParts reports of a part, kept in delta.json so listing a
// delta does not index it.
type listed struct {
	Sessions   []string `json:"sessions"`
	ScoredDocs int64    `json:"scored_docs"`
	TotalLen   int64    `json:"total_len"`
}

func listedOf(st *Part) listed {
	l := listed{ScoredDocs: int64(st.meta.ScoredDocs), TotalLen: int64(st.meta.TotalLen)}
	for s := 0; s < st.nSessions; s++ {
		l.Sessions = append(l.Sessions, st.session(s).id)
	}
	return l
}

// deltaID names a component held as image plus delta.
func deltaID(cks []ckpt) string {
	h := sha256.Sum256([]byte("delta " + fingerprint(cks)))
	return hex.EncodeToString(h[:])
}

func deltasDir(store, image string) string { return filepath.Join(store, "deltas", image) }

// hotPart returns the ID of the part that answers for the hot component
// members, building it if it is missing.
func hotPart(store string, members []ckpt, prev manifestFile) (string, error) {
	id := fingerprint(members)
	if _, err := os.Stat(partDir(store, id)); err == nil {
		return id, nil
	}
	did := deltaID(members)
	if _, err := os.Stat(partDir(store, did)); err == nil {
		return did, nil
	}
	if base, ok := deltaBase(store, members, prev); ok {
		ok, err := appendDelta(store, members, base, did)
		if err != nil || ok {
			return did, err
		}
	}
	return id, buildIndex(store, members, id, partDir(store, id))
}

// deltaBase is the delta of the hot part in prev that members extend and that
// covers the most rows.
func deltaBase(store string, members []ckpt, prev manifestFile) (delta, bool) {
	now := map[string]ckpt{}
	for _, c := range members {
		now[c.Key] = c
	}
	var best delta
	var bestRows int64 = -1
	for _, id := range prev.Hot {
		d, ok := readBase(store, id)
		if !ok {
			continue
		}
		var rows int64
		for _, m := range d.Members {
			c, ok := now[m.Key]
			if !ok || c.Gen != m.Gen || c.LightLen < m.LightLen || c.HeavyLen < m.HeavyLen || c.Rows < m.Rows {
				rows = -1
				break
			}
			rows += m.Rows
		}
		if rows > bestRows {
			best, bestRows = d, rows
		}
	}
	return best, bestRows >= 0
}

// readDelta reads the delta.json of a part dir; ok is false for a full part.
func readDelta(dir string) (delta, bool, error) {
	var d delta
	b, err := os.ReadFile(filepath.Join(dir, deltaFile))
	if errors.Is(err, fs.ErrNotExist) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	return d, true, json.Unmarshal(b, &d)
}

// readBase reads part id's delta, or an empty delta over it when it is a
// full part.
func readBase(store, id string) (delta, bool) {
	dir := partDir(store, id)
	if d, ok, err := readDelta(dir); ok || err != nil {
		return d, err == nil
	}
	var d delta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return d, false
	}
	var m indexMeta
	if json.Unmarshal(b, &m) != nil || len(m.Members) == 0 {
		return d, false
	}
	return delta{Image: id, Members: m.Members}, true
}

func logSize(dir, name string) (int64, error) {
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// appendDelta appends to base's delta logs the rows members hold past it and
// commits part did over them. It reports false, writing nothing, when the
// delta would outgrow foldRatio times the image.
func appendDelta(store string, members []ckpt, base delta, did string) (bool, error) {
	img := partDir(store, base.Image)
	imgLight, err := logSize(img, "light")
	if err != nil {
		return false, nil
	}
	imgHeavy, err := logSize(img, "heavy")
	if err != nil {
		return false, nil
	}
	at := map[string]int{}
	for i, m := range base.Members {
		at[m.Key] = i
	}
	cks := slices.Clone(members)
	sort.Slice(cks, func(i, j int) bool { return cks[i].Key < cks[j].Key })
	grow := base.LightLen + base.HeavyLen
	for _, c := range cks {
		grow += c.LightLen + c.HeavyLen
		if i, ok := at[c.Key]; ok {
			grow -= base.Members[i].LightLen + base.Members[i].HeavyLen
		}
	}
	if grow > foldRatio*(imgLight+imgHeavy) {
		return false, nil
	}

	dd := deltasDir(store, base.Image)
	if err := os.MkdirAll(dd, 0o755); err != nil {
		return false, err
	}
	lightF, err := openLog(filepath.Join(dd, "light"), base.LightLen)
	if err != nil {
		return false, err
	}
	defer lightF.Close()
	heavyF, err := openLog(filepath.Join(dd, "heavy"), base.HeavyLen)
	if err != nil {
		return false, err
	}
	defer heavyF.Close()
	srcF, err := openLog(filepath.Join(dd, "src"), 2*base.Rows)
	if err != nil {
		return false, err
	}
	defer srcF.Close()
	lw := &frameWriter{f: lightF, off: base.LightLen, target: lightFrame, enc: encoder}
	hw := &frameWriter{f: heavyF, off: base.HeavyLen, target: heavyFrame, enc: heavyEncoder}
	next := delta{Image: base.Image, Rows: base.Rows, Members: slices.Clone(base.Members)}
	var srcs []byte
	for _, c := range cks {
		i, ok := at[c.Key]
		var from int64
		if ok {
			from = next.Members[i].LightLen
		} else {
			i = len(next.Members)
			next.Members = append(next.Members, member{})
		}
		next.Members[i] = memberOf(c)
		rows, err := loadSourceRows(store, 0, c, from)
		if err != nil {
			return false, fmt.Errorf("%s: %w", c.Key, err)
		}
		heavy := newHeavyReader(store, []ckpt{c})
		for _, b := range rows {
			hrec, err := heavy.record(b)
			if err != nil {
				heavy.close()
				return false, err
			}
			hf, hi, err := hw.add(hrec)
			if err != nil {
				heavy.close()
				return false, err
			}
			b.row.HeavyFrame, b.row.HeavyIdx = uint64(imgHeavy+hf), uint64(hi)
			if _, _, err := lw.add(b.row.append(nil)); err != nil {
				heavy.close()
				return false, err
			}
			srcs = binary.LittleEndian.AppendUint16(srcs, uint16(i))
			next.Rows++
		}
		heavy.close()
	}
	if err := hw.flush(); err != nil {
		return false, err
	}
	if err := lw.flush(); err != nil {
		return false, err
	}
	if _, err := srcF.Write(srcs); err != nil {
		return false, err
	}
	for _, f := range []*os.File{lightF, heavyF, srcF} {
		if err := f.Sync(); err != nil {
			return false, err
		}
	}
	if err := syncDir(dd); err != nil {
		return false, err
	}
	next.LightLen, next.HeavyLen = lw.off, hw.off
	dir := partDir(store, did) + ".tmp"
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	st, err := openDelta(dir, next)
	if err != nil {
		return false, err
	}
	next.listed = listedOf(st)
	st.Close()
	b, _ := json.Marshal(next)
	if err := os.WriteFile(filepath.Join(dir, deltaFile), b, 0o644); err != nil {
		return false, err
	}
	return true, commitPart(dir, partDir(store, did))
}

// openDelta indexes the image's docs and the delta's rows in memory.
func openDelta(dir string, d delta) (*Part, error) {
	img, err := OpenPartDir(filepath.Join(filepath.Dir(dir), d.Image))
	if err != nil {
		return nil, err
	}
	maps := img.maps
	img.maps = nil
	fail := func(err error) (*Part, error) {
		for _, m := range maps {
			if len(m) > 0 {
				syscall.Munmap(m)
			}
		}
		return nil, err
	}
	dd := deltasDir(filepath.Dir(filepath.Dir(dir)), d.Image)
	var logs [3][]byte
	for i, name := range []string{"light", "heavy", "src"} {
		b, err := mmapFile(filepath.Join(dd, name))
		if err != nil {
			return fail(err)
		}
		maps = append(maps, b)
		logs[i] = b
	}
	lightD, heavyD, srcD := logs[0], logs[1], logs[2]
	if int64(len(lightD)) < d.LightLen || int64(len(heavyD)) < d.HeavyLen || int64(len(srcD)) < 2*d.Rows {
		return fail(fmt.Errorf("%s: delta logs shorter than %s", dd, deltaFile))
	}
	lightD, heavyD, srcD = lightD[:d.LightLen], heavyD[:d.HeavyLen], srcD[:2*d.Rows]

	win := map[string]built{}
	offer := func(b built) {
		if w, ok := win[b.row.UID]; ok && w.src != b.src && d.Members[w.src].Path <= d.Members[b.src].Path {
			return
		}
		win[b.row.UID] = b
	}
	doc := 0
	if err := eachFrame(img.lightB, 0, func(b built) {
		b.src = binary.LittleEndian.Uint16(img.docRec(doc)[10:])
		doc++
		offer(b)
	}); err != nil {
		return fail(err)
	}
	if doc != img.meta.Docs {
		return fail(fmt.Errorf("%s: image holds %d rows for %d docs", dir, doc, img.meta.Docs))
	}
	k := 0
	if err := eachFrame(lightD, int64(len(img.lightB)), func(b built) {
		b.src = binary.LittleEndian.Uint16(srcD[2*k:])
		k++
		offer(b)
	}); err != nil {
		return fail(err)
	}
	if int64(k) != d.Rows {
		return fail(fmt.Errorf("%s: delta holds %d rows, %s says %d", dd, k, deltaFile, d.Rows))
	}
	bySession := map[string][]built{}
	for _, b := range win {
		bySession[b.row.Session] = append(bySession[b.row.Session], b)
	}
	win = nil

	heavy := &heavyReader{logs: []io.ReaderAt{segments{img.heavyB, heavyD}}, recs: map[heavyKey][][]byte{}}
	files := map[string][]byte{}
	place := func(b built) (*Row, int64, int, error) { return b.row, b.lf, b.li, nil }
	payload := func(b built) string { return heavy.payload(built{row: b.row}) }
	write := func(name string, b []byte) error { files[name] = b; return nil }
	meta, lsRecs, err := indexRows(bySession, place, payload, write)
	if err != nil {
		return fail(err)
	}
	if heavy.err != nil {
		return fail(heavy.err)
	}
	if files["ls.json"], err = json.Marshal(lsRecs); err != nil {
		return fail(err)
	}
	meta.Members = d.Members
	mb, _ := json.Marshal(meta)
	files["light"], files["heavy"] = img.lightB, img.heavyB
	get := func(name string) ([]byte, error) {
		b, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("%s: no %s", dir, name)
		}
		return b, nil
	}
	st, err := openPart(mb, get)
	if err != nil {
		return fail(err)
	}
	st.lightD, st.heavyD, st.maps = lightD, heavyD, nil
	for _, m := range maps {
		if len(m) > 0 && !sameStart(m, img.lightB) && !sameStart(m, img.heavyB) && !sameStart(m, lightD) && !sameStart(m, heavyD) {
			syscall.Munmap(m)
			continue
		}
		st.maps = append(st.maps, m)
	}
	return st, nil
}

func sameStart(a, b []byte) bool { return len(a) > 0 && len(b) > 0 && &a[0] == &b[0] }

// eachFrame calls fn with every row of the light log b, located at base plus
// its frame's offset in b.
func eachFrame(b []byte, base int64, fn func(built)) error {
	r := bytes.NewReader(b)
	for off := int64(0); off < int64(len(b)); {
		recs, next, err := readFrame(r, off, nil)
		if err != nil {
			return err
		}
		for i, rec := range recs {
			row := new(Row)
			if _, err := row.decode(rec); err != nil {
				return err
			}
			fn(built{row: row, lf: base + off, li: i})
		}
		off = next
	}
	return nil
}

// gcDeltas removes the delta logs of images not in images.
func gcDeltas(store string, images map[string]bool) {
	ents, _ := os.ReadDir(filepath.Join(store, "deltas"))
	for _, e := range ents {
		if !images[e.Name()] {
			os.RemoveAll(filepath.Join(store, "deltas", e.Name()))
		}
	}
}

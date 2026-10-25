// size0-census — lane S0's box census over an engine store's src/ tree.
// Two read-only subcommands; both print counts only, never corpus content.
//
//	frames <srcdir>  walk every .light/.heavy frame log by its 8-byte frame
//	                 headers (u32 compressed length, u32 record count,
//	                 ingest.go's frameWriter) and print per kind (archive
//	                 member vs raw) and per log type: files, frames, records,
//	                 bytes, and records/frame and bytes/frame percentiles.
//	comp <srcdir>    parse every .ckpt and union-find sources that share a
//	                 session id; print component member-count histograms by
//	                 composition (raw-only / archive-only / mixed) and the
//	                 session overlap counts.
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: size0-census frames|comp <srcdir>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "frames":
		err = frames(os.Args[2])
	case "comp":
		err = comp(os.Args[2])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "size0-census:", err)
		os.Exit(1)
	}
}

type agg struct {
	files    int
	frames   int
	records  int64
	bytes    int64
	badFiles int
	rpf      []uint32 // records per frame
	bpf      []int64  // frame bytes incl the 8-byte header
	fileRecs []int64
	fileByts []int64
}

func (a *agg) pct(p float64) float64 {
	if len(a.rpf) == 0 {
		return 0
	}
	s := make([]uint32, len(a.rpf))
	copy(s, a.rpf)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return float64(s[i])
}

func (a *agg) pctB(p float64) float64 {
	if len(a.bpf) == 0 {
		return 0
	}
	s := make([]int64, len(a.bpf))
	copy(s, a.bpf)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return float64(s[i])
}

func (a *agg) pctF(v []int64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := make([]int64, len(v))
	copy(s, v)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s)-1) * p)
	return float64(s[i])
}

func frames(srcdir string) error {
	aggs := map[string]*agg{}
	paths, err := filepath.Glob(filepath.Join(srcdir, "*.light"))
	if err != nil {
		return err
	}
	heavy, err := filepath.Glob(filepath.Join(srcdir, "*.heavy"))
	if err != nil {
		return err
	}
	paths = append(paths, heavy...)
	fmt.Printf("# files scanned: %d\n", len(paths))
	for _, p := range paths {
		base := filepath.Base(p)
		kind := "raw"
		if strings.Contains(base, "__archive-ch__") {
			kind = "archive"
		}
		typ := "light"
		if strings.HasSuffix(base, ".heavy") {
			typ = "heavy"
		}
		name := kind + "." + typ
		a := aggs[name]
		if a == nil {
			a = &agg{}
			aggs[name] = a
		}
		if err := walkFrames(p, a); err != nil {
			return fmt.Errorf("%s: %w", base, err)
		}
	}
	for _, name := range []string{"archive.light", "archive.heavy", "raw.light", "raw.heavy"} {
		a, ok := aggs[name]
		if !ok || a.files == 0 {
			fmt.Printf("%s\tfiles=0\n", name)
			continue
		}
		sort.Slice(a.fileRecs, func(i, j int) bool { return a.fileRecs[i] < a.fileRecs[j] })
		fmt.Printf("%s\tfiles=%d frames=%d records=%d bytes=%d bad_files=%d\n", name,
			a.files, a.frames, a.records, a.bytes, a.badFiles)
		fmt.Printf("%s\trec/frame p10=%.0f p50=%.0f p90=%.0f mean=%.1f\n", name,
			a.pct(.10), a.pct(.50), a.pct(.90), float64(a.records)/float64(a.frames))
		fmt.Printf("%s\tB/frame p10=%.0f p50=%.0f p90=%.0f mean=%.1f\n", name,
			a.pctB(.10), a.pctB(.50), a.pctB(.90), float64(a.bytes)/float64(a.frames))
		fmt.Printf("%s\trec/file p10=%.0f p50=%.0f p90=%.0f mean=%.1f\n", name,
			a.pctF(a.fileRecs, .10), a.pctF(a.fileRecs, .50), a.pctF(a.fileRecs, .90),
			float64(a.records)/float64(a.files))
		fmt.Printf("%s\tB/file p10=%.0f p50=%.0f p90=%.0f mean=%.1f\n", name,
			a.pctF(a.fileByts, .10), a.pctF(a.fileByts, .50), a.pctF(a.fileByts, .90),
			float64(a.bytes)/float64(a.files))
	}
	return nil
}

// walkFrames reads each frame's 8-byte header and skips past its compressed
// bytes without decoding: frames are u32 clen, u32 count, then clen bytes.
func walkFrames(path string, a *agg) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if size == 0 {
		return nil // no rows ever flushed for this log
	}
	a.files++
	var hdr [8]byte
	var off int64
	var fRecs int64
	var fBytes int64
	for off+8 <= size {
		if _, err := f.ReadAt(hdr[:], off); err != nil {
			a.badFiles++
			return nil
		}
		clen := int64(binary.LittleEndian.Uint32(hdr[0:4]))
		n := binary.LittleEndian.Uint32(hdr[4:8])
		if clen == 0 || off+8+clen > size {
			a.badFiles++
			return nil
		}
		a.frames++
		a.rpf = append(a.rpf, n)
		fb := 8 + clen
		a.bpf = append(a.bpf, fb)
		a.records += int64(n)
		a.bytes += fb
		fRecs += int64(n)
		fBytes += fb
		off += fb
	}
	if off != size {
		a.badFiles++ // trailing partial frame: crash or truncation remnant
	}
	a.fileRecs = append(a.fileRecs, fRecs)
	a.fileByts = append(a.fileByts, fBytes)
	return nil
}

// --- comp ---

type member struct {
	key      string
	archive  bool
	rows     int64
	sessions []string
}

type uf struct{ p []int }

func (u *uf) find(i int) int {
	for u.p[i] != i {
		u.p[i] = u.p[u.p[i]]
		i = u.p[i]
	}
	return i
}

func (u *uf) union(a, b int) { u.p[u.find(a)] = u.find(b) }

func comp(srcdir string) error {
	paths, err := filepath.Glob(filepath.Join(srcdir, "*.ckpt"))
	if err != nil {
		return err
	}
	ms := []member{}
	bySession := map[string][]int{}
	var rawSessions, archSessions, sharedSessions int
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var c struct {
			Key      string   `json:"key"`
			Kind     int      `json:"kind"`
			Rows     int64    `json:"rows"`
			Sessions []string `json:"sessions"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		arch := c.Kind == 2
		i := len(ms)
		ms = append(ms, member{key: c.Key, archive: arch, rows: c.Rows, sessions: c.Sessions})
		for _, s := range c.Sessions {
			if arch {
				archSessions++
			} else {
				rawSessions++
			}
			prev := bySession[s]
			bySession[s] = append(prev, i)
			if len(prev) == 1 {
				sharedSessions++
			}
		}
	}
	u := &uf{p: make([]int, len(ms))}
	for i := range u.p {
		u.p[i] = i
	}
	for _, is := range bySession {
		for _, i := range is[1:] {
			u.union(is[0], i)
		}
	}
	type cinfo struct {
		members int
		raw     int
		arch    int
		rows    int64
	}
	comp := map[int]*cinfo{}
	for i := range ms {
		r := u.find(i)
		c := comp[r]
		if c == nil {
			c = &cinfo{}
			comp[r] = c
		}
		c.members++
		if ms[i].archive {
			c.arch++
		} else {
			c.raw++
		}
		c.rows += ms[i].rows
	}
	sizes := map[int]int{}
	mixed := map[int]int{}
	var multi, multiMembers, mixedCount, mixedMembers int
	var mixedRawMembers, mixedArchMembers int
	for _, c := range comp {
		sizes[c.members]++
		if c.arch > 0 && c.raw > 0 {
			mixed[c.members]++
			mixedCount++
			mixedMembers += c.members
			mixedRawMembers += c.raw
			mixedArchMembers += c.arch
		}
		if c.members > 1 {
			multi++
			multiMembers += c.members
		}
	}
	fmt.Printf("sources=%d (archive_members=%d raw=%d) sessions=%d distinct_sessions=%d\n",
		len(ms), countIf(ms, true), countIf(ms, false), rawSessions+archSessions, len(bySession))
	fmt.Printf("sessions_one_source=%d sessions_shared(2+ sources)=%d\n",
		len(bySession)-countMulti(bySession), sharedSessions)
	fmt.Printf("components=%d multi_member_components=%d members_in_them=%d\n",
		len(comp), multi, multiMembers)
	fmt.Printf("mixed_components(raw+archive)=%d members_in_them=%d (raw=%d archive=%d)\n",
		mixedCount, mixedMembers, mixedRawMembers, mixedArchMembers)
	top := make([]*cinfo, 0, len(comp))
	for _, c := range comp {
		top = append(top, c)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].members > top[j].members })
	for i, c := range top {
		if i == 5 || c.members < 2 {
			break
		}
		fmt.Printf("top component members=%d raw=%d archive=%d rows=%d\n", c.members, c.raw, c.arch, c.rows)
	}
	var overlap int
	for _, is := range bySession {
		var r, a bool
		for _, i := range is {
			if ms[i].archive {
				a = true
			} else {
				r = true
			}
		}
		if r && a {
			overlap++
		}
	}
	fmt.Printf("overlap_sessions(raw and archive hold the session)=%d\n", overlap)
	fmt.Println("member_count\tcomponents\tmixed")
	for _, n := range sortedKeys(sizes) {
		fmt.Printf("%d\t%d\t%d\n", n, sizes[n], mixed[n])
	}
	var rawOnlyRows, archRows int64
	var rawOnly, archOnly int
	for i := range ms {
		if ms[i].archive {
			archRows += ms[i].rows
			archOnly++
		} else {
			rawOnlyRows += ms[i].rows
			rawOnly++
		}
	}
	fmt.Printf("rows raw_members=%d archive_members=%d\n", rawOnlyRows, archRows)
	fmt.Printf("members raw=%d archive=%d\n", rawOnly, archOnly)
	return nil
}

func countIf(ms []member, arch bool) int {
	n := 0
	for _, m := range ms {
		if m.archive == arch {
			n++
		}
	}
	return n
}

func countMulti(bySession map[string][]int) int {
	n := 0
	for _, is := range bySession {
		if len(is) > 1 {
			n++
		}
	}
	return n
}

func sortedKeys(m map[int]int) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	return ks
}

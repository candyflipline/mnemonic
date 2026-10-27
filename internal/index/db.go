package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// manifestFile is CURRENT: the parts that make up the index and the source
// rows get_ingest_status reads. It is written by tmp+fsync+rename and is the
// index's only commit point.
type manifestFile struct {
	Parts   []string   `json:"parts"`
	Hot     []string   `json:"hot,omitempty"`
	Sources []stSource `json:"sources"`
}

// DB is every part of one committed manifest. Parts partition sessions, so a
// session's events, turns and rollups live in exactly one part; only BM25's
// corpus statistics are summed across parts. A part is shared by every DB
// that names it and closes with the last of them. Only the annotations
// section crosses parts: a part may hold annotation entries for a session
// whose rows live in another part, so annPart indexes, per target session,
// every part whose annotations section holds it.
type DB struct {
	parts      []*Part
	ids        []string
	fc         *frameCache
	sessPart   map[string]int
	annPart    map[string][]int
	scoredDocs uint64
	totalLen   uint64
	sources    []stSource
}

func readManifest(store string) (manifestFile, error) {
	var m manifestFile
	b, err := os.ReadFile(filepath.Join(store, "CURRENT"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func partDir(store, id string) string { return filepath.Join(store, "parts", id) }

// PartOpenError is a CURRENT naming a part that no longer opens: the writer's
// GC removed the part after publishing a newer CURRENT, or the store is
// damaged. It is deliberately not fs.ErrNotExist — a missing CURRENT is
// "nothing published yet", this is a stale or broken CURRENT — and Open and
// Refresh re-read CURRENT and retry before surfacing it.
type PartOpenError struct {
	Part string // the part id CURRENT names
	Err  error  // the part's own open failure, message only
}

func (e *PartOpenError) Error() string {
	return fmt.Sprintf("current names part %s, which does not open: %v", e.Part, e.Err)
}

func OpenDB(store string) (*DB, error) { return openDB(store, nil, nil) }

// openDB opens the parts CURRENT names, taking from prev each one it opened
// from the same dir. A committed part dir is never written again, and the
// delta logs a delta part reads are truncated only to the length the delta
// in CURRENT names, so a part taken from prev reads what a fresh open would, and a hot delta is indexed once, not once per Refresh.
// The caller keeps prev pinned until openDB returns. opened is parts the
// caller opened, by id, and hands over: each one CURRENT names joins the DB
// when its stamp matches the dir's, and every other one closes.
func openDB(store string, prev *DB, opened map[string]*Part) (*DB, error) {
	defer func() { closeParts(opened) }()
	m, err := readManifest(store)
	if err != nil {
		return nil, err
	}
	had := map[string]*Part{}
	fc := newFrameCache()
	if prev != nil && prev.fc != nil {
		fc = prev.fc
		for i, id := range prev.ids {
			had[id] = prev.parts[i]
		}
	}
	parts := make([]*Part, len(m.Parts))
	taken := make([]bool, len(m.Parts))
	errs := make([]error, len(m.Parts))
	eachIndex(len(m.Parts), func(i int) {
		dir := partDir(store, m.Parts[i])
		if p := had[m.Parts[i]]; p != nil {
			if at, err := dirStampOf(dir); err == nil && at == p.at {
				parts[i], taken[i] = p, true
				return
			}
		}
		if p := opened[m.Parts[i]]; p != nil {
			if at, err := dirStampOf(dir); err == nil && at == p.at {
				parts[i] = p
				return
			}
		}
		parts[i], errs[i] = OpenPartDir(dir)
	})
	for i, p := range parts {
		if p != nil && opened[m.Parts[i]] == p {
			delete(opened, m.Parts[i])
		}
	}
	for i, err := range errs {
		if err != nil {
			for j, p := range parts {
				if p != nil && !taken[j] {
					p.Close()
				}
			}
			return nil, &PartOpenError{Part: m.Parts[i], Err: err}
		}
	}
	db := newDB(parts, fc)
	db.ids = m.Parts
	db.sources = m.Sources
	return db, nil
}

// OpenParts is a DB over parts the caller opened. It answers every tool but
// get_ingest_status, which has no source rows here.
func OpenParts(parts []*Part) (*DB, error) {
	return newDB(parts, newFrameCache()), nil
}

// newDB holds each of parts once more. A part another DB already holds keeps
// its frame cache, which is fc: the DB it came from handed fc on.
func newDB(parts []*Part, fc *frameCache) *DB {
	db := &DB{parts: parts, fc: fc, sessPart: map[string]int{}, annPart: map[string][]int{}}
	for i, st := range parts {
		if st.refs.Add(1) == 1 {
			st.fc = fc
		}
		db.scoredDocs += st.meta.ScoredDocs
		db.totalLen += st.meta.TotalLen
		for s := 0; s < st.nSessions; s++ {
			db.sessPart[st.session(s).id] = i
		}
		for sid := range st.annAt.of(st.annB) {
			db.annPart[sid] = append(db.annPart[sid], i)
		}
	}
	return db
}

func (db *DB) Close() {
	for _, st := range db.parts {
		st.release()
	}
}

// CurrentStamp names the CURRENT content batch last opened: the first eight
// bytes of its SHA-256, so a commit that reuses CURRENT's inode still changes it.
func CurrentStamp(store string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(store, "CURRENT"))
	if err != nil {
		return 0, false
	}
	h := sha256.Sum256(b)
	return binary.LittleEndian.Uint64(h[:8]), true
}

func (db *DB) part(sessionID string) (*Part, bool) {
	i, ok := db.sessPart[sessionID]
	if !ok {
		return nil, false
	}
	return db.parts[i], true
}

func (db *DB) sessionOfUID(uid string) (string, bool, error) {
	for _, st := range db.parts {
		if s, ok, err := st.sessionOfUID(uid); err != nil || ok {
			return s, ok, err
		}
	}
	return "", false, nil
}

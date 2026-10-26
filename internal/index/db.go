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
// corpus statistics are summed across parts, and only the annotations
// section: a part may hold annotation entries for a session whose rows live
// in another part, so annPart indexes, per target session, every part whose
// annotations section holds it.
type DB struct {
	parts      []*Part
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

func OpenDB(store string) (*DB, error) {
	m, err := readManifest(store)
	if err != nil {
		return nil, err
	}
	parts := make([]*Part, len(m.Parts))
	errs := make([]error, len(m.Parts))
	eachIndex(len(m.Parts), func(i int) {
		parts[i], errs[i] = OpenPartDir(partDir(store, m.Parts[i]))
	})
	for i, err := range errs {
		if err != nil {
			for _, p := range parts {
				if p != nil {
					p.Close()
				}
			}
			return nil, &PartOpenError{Part: m.Parts[i], Err: err}
		}
	}
	db, err := OpenParts(parts)
	if err != nil {
		return nil, err
	}
	db.sources = m.Sources
	return db, nil
}

// OpenParts is a DB over parts the caller opened. It answers every tool but
// get_ingest_status, which has no source rows here.
func OpenParts(parts []*Part) (*DB, error) {
	db := &DB{parts: parts, sessPart: map[string]int{}, annPart: map[string][]int{}}
	fc := newFrameCache()
	for i, st := range parts {
		st.fc = fc
		db.scoredDocs += st.meta.ScoredDocs
		db.totalLen += st.meta.TotalLen
		for s := 0; s < st.nSessions; s++ {
			db.sessPart[st.session(s).id] = i
		}
		for sid := range st.annAt.of(st.annB) {
			db.annPart[sid] = append(db.annPart[sid], i)
		}
	}
	return db, nil
}

func (db *DB) Close() {
	for _, st := range db.parts {
		st.Close()
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

package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// manifestFile is CURRENT: the parts that make up the index and the source
// rows get_ingest_status reads. It is written by tmp+fsync+rename and is the
// index's only commit point.
type manifestFile struct {
	Parts   []string   `json:"parts"`
	Sources []stSource `json:"sources"`
}

// DB is every part of one committed manifest. Parts partition sessions, so a
// session's events, turns and rollups live in exactly one part; only BM25's
// corpus statistics are summed across parts.
type DB struct {
	parts      []*Part
	sessPart   map[string]int
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

func OpenDB(store string) (*DB, error) {
	m, err := readManifest(store)
	if err != nil {
		return nil, err
	}
	db := &DB{sessPart: map[string]int{}, sources: m.Sources}
	fc := newFrameCache()
	for i, id := range m.Parts {
		st, err := openPart(store, partDir(store, id), fc)
		if err != nil {
			return nil, err
		}
		db.parts = append(db.parts, st)
		db.scoredDocs += st.meta.ScoredDocs
		db.totalLen += st.meta.TotalLen
		for s := 0; s < st.nSessions; s++ {
			db.sessPart[st.session(s).id] = i
		}
	}
	return db, nil
}

func (db *DB) Close() {
	for _, st := range db.parts {
		st.close()
	}
}

// CurrentStamp names the CURRENT file batch last opened: every commit renames
// a new inode into place.
func CurrentStamp(store string) (uint64, bool) {
	fi, err := os.Stat(filepath.Join(store, "CURRENT"))
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ino, true
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

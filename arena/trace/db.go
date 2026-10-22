package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"syscall"
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
	parts := make([]*Part, 0, len(m.Parts))
	for _, id := range m.Parts {
		st, err := OpenPartDir(partDir(store, id))
		if err != nil {
			for _, p := range parts {
				p.Close()
			}
			return nil, err
		}
		parts = append(parts, st)
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
	db := &DB{parts: parts, sessPart: map[string]int{}}
	fc := newFrameCache()
	for i, st := range parts {
		st.fc = fc
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
		st.Close()
	}
}

// LocalPart is one part of the store's CURRENT, as the keeper lists it.
type LocalPart struct {
	ID         string
	Dir        string
	Hot        bool
	Sessions   []string
	ScoredDocs int64
	TotalLen   int64
}

// ListParts lists the parts CURRENT names and the source rows
// get_ingest_status reads.
func ListParts(store string) ([]LocalPart, json.RawMessage, error) {
	m, err := readManifest(store)
	if err != nil {
		return nil, nil, err
	}
	sources, err := json.Marshal(m.Sources)
	if err != nil {
		return nil, nil, err
	}
	out := make([]LocalPart, 0, len(m.Parts))
	for _, id := range m.Parts {
		dir := partDir(store, id)
		st, err := OpenPartDir(dir)
		if err != nil {
			return nil, nil, err
		}
		lp := LocalPart{ID: id, Dir: dir, Hot: slices.Contains(m.Hot, id), ScoredDocs: int64(st.meta.ScoredDocs), TotalLen: int64(st.meta.TotalLen)}
		for s := 0; s < st.nSessions; s++ {
			lp.Sessions = append(lp.Sessions, st.session(s).id)
		}
		st.Close()
		out = append(out, lp)
	}
	return out, sources, nil
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

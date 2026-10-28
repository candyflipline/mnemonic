package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type manifestFile struct {
	Parts   []string   `json:"parts"`
	Hot     []string   `json:"hot,omitempty"`
	Sources []stSource `json:"sources"`
}

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

type noCurrentError struct{ error }

func (e noCurrentError) Unwrap() error { return e.error }

func readManifest(store string) (manifestFile, error) {
	var m manifestFile
	b, err := os.ReadFile(filepath.Join(store, "CURRENT"))
	if errors.Is(err, fs.ErrNotExist) {
		return m, noCurrentError{err}
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func partDir(store, id string) string { return filepath.Join(store, "parts", id) }

func reusable(p *Part, dir string) bool {
	if p == nil {
		return false
	}
	at, err := dirStampOf(dir)
	return err == nil && at == p.at
}

type PartOpenError struct {
	Part string
	Err  error
}

func (e *PartOpenError) Error() string {
	return fmt.Sprintf("current names part %s, which does not open: %v", e.Part, e.Err)
}

func OpenDB(store string) (*DB, error) { return openDB(store, nil, nil) }

func openDB(store string, pin func() (*DB, func()), opened map[string]*Part) (*DB, error) {
	var prev *DB
	if pin != nil {
		var release func()
		prev, release = pin()
		defer release()
	}
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
		if p := had[m.Parts[i]]; reusable(p, dir) {
			parts[i], taken[i] = p, true
			return
		}
		if p := opened[m.Parts[i]]; reusable(p, dir) {
			parts[i], taken[i] = p, true
			return
		}
		parts[i], errs[i] = OpenPartDir(dir)
	})
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

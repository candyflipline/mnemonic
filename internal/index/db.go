package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/candyflipline/mnemonic/internal/schema"
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
// corpus statistics are summed across parts. The annotations relation rides
// beside the parts: annotation rows are in no part, and every part of the DB
// answers a session's annotations from the relation.
type DB struct {
	parts      []*Part
	sessPart   map[string]int
	anns       map[string][]schema.Annotation
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

func OpenDB(store string) (*DB, error) { return openDB(store, nil) }

// openDB is OpenDB with the annotations relation handed in: the boot's cached
// one, reused when the annotation checkpoints did not change. anns is
// attached as-is and must not be mutated afterwards; nil builds the relation
// from the store's annotation checkpoints.
func openDB(store string, anns map[string][]schema.Annotation) (*DB, error) {
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
	if anns == nil {
		var cks []ckpt
		cks, err = listAnnCkpts(store)
		if err == nil {
			anns, err = buildAnns(store, cks)
		}
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	db.attachAnns(anns)
	return db, nil
}

// attachAnns gives the DB and its parts the annotations relation. The parts
// are opened before the relation exists, so it is wired in here; the export
// answers a part's annotations through it (Part.annOf).
func (db *DB) attachAnns(anns map[string][]schema.Annotation) {
	db.anns = anns
	for _, st := range db.parts {
		st.annRel = anns
	}
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

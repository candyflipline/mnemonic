// Package store is the server side of the wire protocol.
package store

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// segment bounds a machine, harness or src_id to one safe path piece; "." and
// ".." pass the class but must never resolve as path elements.
var segment = regexp.MustCompile(`^[a-z0-9._-]+$`)

func validSegment(s string) bool {
	return s != "." && s != ".." && segment.MatchString(s)
}

// manifestBody is what a Key.Manifest file holds: the wire.Source the protocol
// answers with, plus zst_size, the raw-file length Source.Size is known to
// cover. An append truncates the zst file to zst_size before adding its frame,
// so a crash between the raw write and the manifest write costs only the torn
// tail, never stored data.
type manifestBody struct {
	wire.Source
	ZstSize int64 `json:"zst_size"`
}

// Store holds raw sources under DataDir per internal/layout.
type Store struct {
	DataDir string

	mu    sync.Mutex
	locks map[layout.Key]*sync.Mutex
}

// lockKey serialises writers of one key; it returns the unlock func.
func (s *Store) lockKey(k layout.Key) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[layout.Key]*sync.Mutex)
	}
	l := s.locks[k]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[k] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Handler serves GET /v1/manifest and PUT /v1/raw/... for the author that
// auth.Middleware put on the context. The mux answers 405 for a known path
// with the wrong method and 404 for everything else.
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /v1/manifest", http.HandlerFunc(s.serveManifest))
	mux.Handle("PUT /v1/raw/{machine}/{harness}/{src_id}", http.HandlerFunc(s.servePut))
	return mux
}

func (s *Store) serveManifest(w http.ResponseWriter, r *http.Request) {
	if !requireAuthor(w, r) {
		return
	}
	author := auth.Author(r.Context())
	machine := r.URL.Query().Get("machine")
	if !validSegment(machine) {
		http.Error(w, "bad machine", http.StatusBadRequest)
		return
	}
	files, err := filepath.Glob(filepath.Join(s.DataDir, "raw",
		"author="+author, "machine="+machine, "harness=*", "*.json"))
	if err != nil {
		http.Error(w, "list manifests", http.StatusInternalServerError)
		return
	}
	m := wire.Manifest{Author: author, Machine: machine, Sources: []wire.Source{}}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var src wire.Source
		if json.Unmarshal(b, &src) == nil {
			m.Sources = append(m.Sources, src)
		}
	}
	sort.Slice(m.Sources, func(i, j int) bool { return m.Sources[i].SrcID < m.Sources[j].SrcID })
	writeJSON(w, http.StatusOK, m)
}

func (s *Store) servePut(w http.ResponseWriter, r *http.Request) {
	if !requireAuthor(w, r) {
		return
	}
	author := auth.Author(r.Context())
	machine, harness, srcID := r.PathValue("machine"), r.PathValue("harness"), r.PathValue("src_id")
	for _, seg := range [...]string{machine, harness, srcID} {
		if !validSegment(seg) {
			http.Error(w, "bad path segment", http.StatusBadRequest)
			return
		}
	}
	q := r.URL.Query()
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "bad offset", http.StatusBadRequest)
		return
	}
	headHash, sourceName, srcPath := q.Get("head_hash"), q.Get("source_name"), q.Get("path")
	if headHash == "" || sourceName == "" || srcPath == "" {
		http.Error(w, "missing query parameter", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		http.Error(w, "body must be non-empty whole lines", http.StatusBadRequest)
		return
	}

	key := layout.Key{Author: author, Machine: machine, Harness: harness, SrcID: srcID}
	defer s.lockKey(key)()

	if offset == 0 {
		s.replace(w, key, headHash, sourceName, srcPath, body)
		return
	}
	s.append(w, key, offset, headHash, body)
}

// replaceRaw writes body as the whole raw file and returns the compressed
// length; the caller owns the manifest, so a test can crash between the two.
func (s *Store) replaceRaw(k layout.Key, body []byte) (int64, error) {
	return writeZst(k.Raw(s.DataDir), body)
}

// replace stores body as the whole raw file, then the manifest: data first, so
// a manifest never describes bytes that are not on disk.
func (s *Store) replace(w http.ResponseWriter, k layout.Key, headHash, sourceName, srcPath string, body []byte) {
	zst, err := s.replaceRaw(k, body)
	if err != nil {
		http.Error(w, "write raw", http.StatusInternalServerError)
		return
	}
	m := manifestBody{
		Source: wire.Source{
			SrcID:      k.SrcID,
			Harness:    k.Harness,
			SourceName: sourceName,
			Path:       srcPath,
			Size:       int64(len(body)),
			HeadHash:   headHash,
			UpdatedMs:  time.Now().UnixMilli(),
		},
		ZstSize: zst,
	}
	if err := writeManifest(k.Manifest(s.DataDir), m); err != nil {
		http.Error(w, "write manifest", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, m.Source)
}

func (s *Store) append(w http.ResponseWriter, k layout.Key, offset int64, headHash string, body []byte) {
	m, err := readManifest(k.Manifest(s.DataDir))
	if err != nil {
		// Nothing stored (or unreadable): there is no offset to append from.
		writeJSON(w, http.StatusConflict, wire.Conflict{})
		return
	}
	if offset != m.Size || headHash != m.HeadHash {
		writeJSON(w, http.StatusConflict, wire.Conflict{Size: m.Size, HeadHash: m.HeadHash})
		return
	}
	zst, err := appendZst(k.Raw(s.DataDir), m.ZstSize, body)
	if err != nil {
		http.Error(w, "append raw", http.StatusInternalServerError)
		return
	}
	m.Size += int64(len(body))
	m.UpdatedMs = time.Now().UnixMilli()
	m.ZstSize = zst
	if err := writeManifest(k.Manifest(s.DataDir), *m); err != nil {
		http.Error(w, "write manifest", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, m.Source)
}

// requireAuthor answers 401 unless Middleware ran; the author never comes from
// the request.
func requireAuthor(w http.ResponseWriter, r *http.Request) bool {
	if auth.Author(r.Context()) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeZst replaces raw with one zstd frame of body and returns the compressed
// length. Temp then rename: a reader never sees a partial frame.
func writeZst(raw string, body []byte) (int64, error) {
	dir := filepath.Dir(raw)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(raw)+".*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	zst, err := writeFrame(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if err = os.Rename(tmp, raw); err != nil {
		return 0, err
	}
	syncDir(dir)
	return zst, nil
}

// appendZst adds one zstd frame after dropping any bytes past zst, the length
// the manifest vouches for: a crash after a raw write but before its manifest
// write leaves exactly such a tail.
func appendZst(raw string, zst int64, body []byte) (int64, error) {
	f, err := os.OpenFile(raw, os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := f.Truncate(zst); err != nil {
		return 0, err
	}
	return writeFrame(f, body)
}

// writeFrame encodes body as one independent zstd frame at the file's end and
// returns the file length, which is the new zst_size.
func writeFrame(f *os.File, body []byte) (int64, error) {
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return 0, err
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		return 0, err
	}
	_, err = zw.Write(body)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func readManifest(path string) (*manifestBody, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifestBody
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeManifest(path string, m manifestBody) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir makes a rename in dir survive a crash; fsyncing the file alone does
// not.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

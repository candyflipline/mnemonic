package index

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

const controlHarness = "control"

type visibility string

const (
	visPrivate visibility = "private"
	visTeam    visibility = "team"
)

type controlAction string

const (
	actVisibility controlAction = "visibility"
	actDelete     controlAction = "delete"
)

type controlRecord struct {
	SessionID  string        `json:"session_id"`
	Action     controlAction `json:"action"`
	Visibility visibility    `json:"visibility,omitempty"`
	TS         time.Time     `json:"ts"`
	RequestKey string        `json:"request_key"`
	Writer     string        `json:"writer,omitempty"`
}

var writer = func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return bi.Main.Version
}()

func parseControl(line []byte) (controlRecord, error) {
	var r controlRecord
	if err := json.Unmarshal(line, &r); err != nil {
		return r, err
	}
	switch {
	case r.SessionID == "" || r.RequestKey == "" || r.TS.IsZero():
		return r, fmt.Errorf("control record lacks session_id, request_key or ts")
	case r.Action == actDelete && r.Visibility == "":
	case r.Action == actVisibility && slices.Contains([]visibility{visPrivate, visTeam}, r.Visibility):
	default:
		return r, fmt.Errorf("control record action %q visibility %q", r.Action, r.Visibility)
	}
	return r, nil
}

type sessionControl struct {
	visibility visibility
	ts         time.Time
	key        string
	deleted    bool
}

// Controls is every session control record in raw, the copy reads consult:
// a record counts from the moment record returns, before any converge.
type Controls struct {
	write    sync.Mutex
	mu       sync.RWMutex
	sessions map[string]sessionControl
}

func (c *Controls) apply(r controlRecord) {
	s := c.sessions[r.SessionID]
	switch r.Action {
	case actDelete:
		s.deleted = true
	case actVisibility:
		if r.TS.After(s.ts) || (r.TS.Equal(s.ts) && r.RequestKey > s.key) {
			s.visibility, s.ts, s.key = r.Visibility, r.TS, r.RequestKey
		}
	}
	c.sessions[r.SessionID] = s
}

// LoadControls reads every control source under dataDir up to the length
// its manifest covers, so a torn append the manifest never recorded is not
// read.
func LoadControls(dataDir string) (*Controls, error) {
	c := &Controls{sessions: map[string]sessionControl{}}
	manifests, err := filepath.Glob(filepath.Join(dataDir, "raw", "author=*", "machine="+store.ServerMachine, "harness="+controlHarness, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		if err := c.loadSource(m); err != nil {
			return nil, fmt.Errorf("controls %s: %w", m, err)
		}
	}
	return c, nil
}

func (c *Controls) loadSource(manifest string) error {
	b, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	var m struct {
		ZstSize int64 `json:"zst_size"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	raw, err := os.Open(strings.TrimSuffix(manifest, ".json") + ".jsonl.zst")
	if err != nil {
		return err
	}
	defer raw.Close()
	zr, err := zstd.NewReader(io.NewSectionReader(raw, 0, m.ZstSize))
	if err != nil {
		return err
	}
	defer zr.Close()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		r, err := parseControl(sc.Bytes())
		if err != nil {
			return err
		}
		c.apply(r)
	}
	return sc.Err()
}

func (c *Controls) record(st *store.Committer, author string, r controlRecord) (duplicate bool, err error) {
	c.write.Lock()
	defer c.write.Unlock()
	r.TS, r.Writer = time.Now().UTC(), writer
	line, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	if _, duplicate, err = st.Append(store.Today(author, controlHarness, ""), r.RequestKey, append(line, '\n')); err != nil || duplicate {
		return duplicate, err
	}
	c.mu.Lock()
	c.apply(r)
	c.mu.Unlock()
	return false, nil
}

type controlSets struct{ deleted, private []string }

func (c *Controls) sets() controlSets {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var s controlSets
	for id, sc := range c.sessions {
		switch {
		case sc.deleted:
			s.deleted = append(s.deleted, id)
		case sc.visibility == visPrivate:
			s.private = append(s.private, id)
		}
	}
	slices.Sort(s.deleted)
	slices.Sort(s.private)
	return s
}

func (c *Controls) deleted(sessionID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[sessionID].deleted
}

type controlsKey struct{}

func WithControls(ctx context.Context, c *Controls) context.Context {
	return context.WithValue(ctx, controlsKey{}, c)
}

func controlsFrom(ctx context.Context) *Controls {
	c, _ := ctx.Value(controlsKey{}).(*Controls)
	return c
}

type controlArgs struct {
	SessionID  string     `json:"session_id"`
	Visibility visibility `json:"visibility"`
	RequestKey string     `json:"request_key"`
}

func parseControlArgs(raw json.RawMessage) (controlArgs, string, error) {
	var a controlArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, "", argErrorf("invalid arguments: %s", err)
	}
	id, err := parseMcpID(a.SessionID)
	if err != nil {
		return a, "", err
	}
	if id.kind != kindSession {
		return a, "", argErrorf("session_id must be a session id")
	}
	if len(a.RequestKey) > 128 {
		return a, "", argErrorf("request_key must be at most 128 characters")
	}
	return a, id.session, nil
}

// actor is the caller on ctx when it may act on session: every author of
// the session is in the caller's tenant, and the caller is its sole author or
// holds an admin token. Any other session is not found.
func actor(ctx context.Context, db *DB, c *Controls, session string) (auth.Identity, error) {
	id, ok := auth.From(ctx)
	if !ok || c == nil || store.From(ctx) == nil {
		return id, errors.New("index: control tools need an identity, controls and a store on ctx")
	}
	return actorOn(id, db.sessionAuthors(session))
}

func actorOn(id auth.Identity, authors []string) (auth.Identity, error) {
	members := id.Members()
	if len(authors) == 0 || slices.ContainsFunc(authors, func(a string) bool { return !members.Has(a) }) {
		return id, argErrorf("session not found")
	}
	if !slices.Equal(authors, []string{id.Author()}) && !id.Can(auth.Admin) {
		return id, argErrorf("only the session's author or an admin token may change it")
	}
	return id, nil
}

func recordKey(action controlAction, session string, arg string, client string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{string(action), session, arg, client}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func randomKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

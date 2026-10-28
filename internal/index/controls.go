package index

import (
	"bufio"
	"bytes"
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
	"github.com/candyflipline/mnemonic/internal/wire"
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
	actLink       controlAction = "link"
	actJoin       controlAction = "join"
	actRevoke     controlAction = "revoke"
)

type controlRecord struct {
	SessionID      string        `json:"session_id,omitempty"`
	Action         controlAction `json:"action"`
	Visibility     visibility    `json:"visibility,omitempty"`
	LinkID         string        `json:"link_id,omitempty"`
	Kind           linkKind      `json:"kind,omitempty"`
	TokenSHA256    string        `json:"token_sha256,omitempty"`
	SessionIDs     []string      `json:"session_ids,omitempty"`
	SessionAuthors []string      `json:"session_authors,omitempty"`
	Title          string        `json:"title,omitempty"`
	Note           string        `json:"note,omitempty"`
	Cutoff         time.Time     `json:"cutoff,omitzero"`
	Expires        time.Time     `json:"expires,omitzero"`
	InviteAuthor   string        `json:"invite_author,omitempty"`
	Author         string        `json:"author,omitempty"`
	Tenant         string        `json:"tenant,omitempty"`
	Key            string        `json:"key,omitempty"`
	Machine        string        `json:"machine,omitempty"`
	TS             time.Time     `json:"ts"`
	RequestKey     string        `json:"request_key"`
	Writer         string        `json:"writer,omitempty"`
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

var recordShapes = map[string]struct{ required, optional []string }{
	"delete":      {required: []string{"session_id"}},
	"visibility":  {required: []string{"session_id", "visibility"}},
	"link/read":   {required: []string{"link_id", "token_sha256", "session_ids", "session_authors", "cutoff", "expires", "author", "tenant"}, optional: []string{"title", "note"}},
	"link/invite": {required: []string{"link_id", "token_sha256", "invite_author", "expires", "author", "tenant"}},
	"join":        {required: []string{"link_id", "key", "machine"}},
	"revoke":      {required: []string{"link_id"}},
}

func (r controlRecord) set() []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"session_id", r.SessionID != ""}, {"visibility", r.Visibility != ""}, {"link_id", r.LinkID != ""},
		{"token_sha256", r.TokenSHA256 != ""}, {"session_ids", r.SessionIDs != nil}, {"session_authors", r.SessionAuthors != nil}, {"title", r.Title != ""},
		{"note", r.Note != ""}, {"cutoff", !r.Cutoff.IsZero()}, {"expires", !r.Expires.IsZero()},
		{"invite_author", r.InviteAuthor != ""}, {"author", r.Author != ""}, {"tenant", r.Tenant != ""},
		{"key", r.Key != ""}, {"machine", r.Machine != ""},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

func parseControl(line []byte) (controlRecord, error) {
	var r controlRecord
	if err := json.Unmarshal(line, &r); err != nil {
		return r, err
	}
	if r.RequestKey == "" || r.TS.IsZero() {
		return r, fmt.Errorf("control record lacks request_key or ts")
	}
	shape := string(r.Action)
	if r.Action == actLink {
		shape += "/" + string(r.Kind)
	} else if r.Kind != "" {
		shape += "/"
	}
	want, ok := recordShapes[shape]
	if !ok {
		return r, fmt.Errorf("control record action %q kind %q", r.Action, r.Kind)
	}
	set := r.set()
	for _, f := range want.required {
		if !slices.Contains(set, f) {
			return r, fmt.Errorf("control record %s lacks %s", shape, f)
		}
	}
	for _, f := range set {
		if !slices.Contains(want.required, f) && !slices.Contains(want.optional, f) {
			return r, fmt.Errorf("control record %s carries %s", shape, f)
		}
	}
	switch {
	case r.Visibility != "" && !slices.Contains([]visibility{visPrivate, visTeam}, r.Visibility):
		return r, fmt.Errorf("control record visibility %q", r.Visibility)
	case r.LinkID != "" && !isHex(r.LinkID, linkIDLen):
		return r, fmt.Errorf("control record link_id %q is not %d hex digits", r.LinkID, linkIDLen)
	case r.TokenSHA256 != "" && !isHex(r.TokenSHA256, 2*sha256.Size):
		return r, fmt.Errorf("control record token_sha256 is not %d hex digits", 2*sha256.Size)
	case slices.Contains(r.SessionIDs, "") || (r.SessionIDs != nil && len(r.SessionIDs) == 0):
		return r, fmt.Errorf("control record session_ids holds an empty id or none")
	case len(r.SessionAuthors) != len(r.SessionIDs) || slices.ContainsFunc(r.SessionAuthors, func(a string) bool { return !wire.ValidSegment(a) }):
		return r, fmt.Errorf("control record session_authors must name one author per session id")
	case r.InviteAuthor != "" && !wire.ValidSegment(r.InviteAuthor):
		return r, fmt.Errorf("control record invite_author %q", r.InviteAuthor)
	case r.Machine != "" && !wire.ValidSegment(r.Machine):
		return r, fmt.Errorf("control record machine %q", r.Machine)
	}
	if r.Key != "" {
		if _, err := auth.ParseKey(r.Key); err != nil {
			return r, fmt.Errorf("control record: %w", err)
		}
	}
	return r, nil
}

func isHex(s string, n int) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s) == n && strings.ToLower(s) == s
}

type sessionControl struct {
	visibility visibility
	lww
	deleted bool
}

type lww struct {
	ts  time.Time
	key string
}

func (l lww) losesTo(r controlRecord) bool {
	return r.TS.After(l.ts) || (r.TS.Equal(l.ts) && r.RequestKey > l.key)
}

type Controls struct {
	write    sync.Mutex
	mu       sync.RWMutex
	sessions map[string]sessionControl
	links    map[string]*link
	byToken  map[[sha256.Size]byte]*link
	byKey    map[string]*link
}

func (c *Controls) apply(r controlRecord) {
	switch r.Action {
	case actLink, actJoin, actRevoke:
		c.applyLink(r)
		return
	}
	s := c.sessions[r.SessionID]
	switch r.Action {
	case actDelete:
		s.deleted = true
	case actVisibility:
		if s.losesTo(r) {
			s.visibility, s.lww = r.Visibility, lww{r.TS, r.RequestKey}
		}
	}
	c.sessions[r.SessionID] = s
}

// LoadControls reads every control source under dataDir up to the length
// its manifest covers, so a torn append the manifest never recorded is not
// read.
func LoadControls(dataDir string) (*Controls, error) {
	c := &Controls{sessions: map[string]sessionControl{}, links: map[string]*link{}, byToken: map[[sha256.Size]byte]*link{}, byKey: map[string]*link{}}
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
	return c.recordLocked(st, author, r)
}

func (c *Controls) recordLocked(st *store.Committer, author string, r controlRecord) (duplicate bool, err error) {
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

func actor(c *Caller, db *DB, session string) (auth.Identity, error) {
	if c == nil {
		return auth.Identity{}, errors.New("index: control tools need an identity, controls and a store")
	}
	return actorOn(c.Identity, db.sessionAuthors(session))
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

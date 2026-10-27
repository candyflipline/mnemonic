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
	"github.com/candyflipline/mnemonic/internal/wallet"
)

// controlHarness is the server harness session controls are appended to.
const controlHarness = "control"

type visibility string

const (
	visPrivate visibility = "private"
	visTeam    visibility = "team"
	visLink    visibility = "link"
)

type controlAction string

const (
	actVisibility controlAction = "visibility"
	actDelete     controlAction = "delete"
	actShare      controlAction = "share"
	actUnshare    controlAction = "unshare"
)

// controlRecord is one raw line of a control source. The actor is the
// source's author, from its path; Writer is the build that appended it. A
// field this build does not know is one a later build added, and is ignored.
type controlRecord struct {
	SessionID  string        `json:"session_id"`
	Action     controlAction `json:"action"`
	Visibility visibility    `json:"visibility,omitempty"`
	Wallet     string        `json:"wallet,omitempty"`
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
	case r.Action == actDelete && r.Visibility == "" && r.Wallet == "":
	case (r.Action == actShare || r.Action == actUnshare) && r.Visibility == "" && validWallet(r.Wallet):
	case r.Action == actVisibility && slices.Contains([]visibility{visPrivate, visTeam, visLink}, r.Visibility):
	default:
		return r, fmt.Errorf("control record action %q visibility %q", r.Action, r.Visibility)
	}
	return r, nil
}

// validWallet is whether s is a base58 wallet address as share_session
// stores it: wallet.ParseAddress accepts it and String prints it back.
func validWallet(s string) bool {
	a, err := wallet.ParseAddress(s)
	return err == nil && a.String() == s
}

type sessionControl struct {
	visibility visibility
	ts         time.Time
	key        string
	deleted    bool
	shares     map[wallet.Address]stamp
}

// stamp is the latest share or unshare of one session with one wallet, by the
// same last-writer-wins order as visibility: later ts, then greater key.
type stamp struct {
	shared bool
	ts     time.Time
	key    string
}

func (s stamp) before(r controlRecord) bool {
	return r.TS.After(s.ts) || (r.TS.Equal(s.ts) && r.RequestKey > s.key)
}

// Controls is every session control record in raw, the copy reads consult:
// a record counts from the moment record returns, before any converge.
// byWallet is derived from sessions by apply alone, under the same lock: the
// sessions each wallet's latest stamp shares, so the handshake asks one map.
type Controls struct {
	write    sync.Mutex
	mu       sync.RWMutex
	sessions map[string]sessionControl
	byWallet map[wallet.Address]map[string]bool
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
	case actShare, actUnshare:
		// TODO: w := wallet.ParseAddress(r.Wallet) (parseControl checked it);
		// if s.shares[w].before(r): set s.shares[w] = stamp{r.Action == actShare, r.TS, r.RequestKey}
		// and set or clear c.byWallet[w][r.SessionID], deleting an empty inner map.
		panic("not implemented")
	}
	c.sessions[r.SessionID] = s
}

// Admits is whether w may connect: some session not deleted is shared with
// it now. The handshake calls it, through server.Identify, on every new
// connection.
func (c *Controls) Admits(w wallet.Address) bool {
	// TODO: RLock; any id in c.byWallet[w] with !c.sessions[id].deleted.
	panic("not implemented")
}

// sharedWith is the sessions shared with w now, deleted ones included (the
// filter hides those), as a set the caller owns.
func (c *Controls) sharedWith(w wallet.Address) map[string]bool {
	panic("not implemented")
}

// walletsOf is the wallets session is shared with now, sorted base58, for
// share_session's answer.
func (c *Controls) walletsOf(session string) []string {
	panic("not implemented")
}

// LoadControls reads every control source under dataDir up to the length
// its manifest covers, so a torn append the manifest never recorded is not
// read.
func LoadControls(dataDir string) (*Controls, error) {
	c := &Controls{sessions: map[string]sessionControl{}, byWallet: map[wallet.Address]map[string]bool{}}
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

// record appends r as author's, stamped now, and applies it. A request key
// already appended changes nothing: its record was applied when appended.
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

// controlSets is a snapshot of the sessions controls hide or open, each
// sorted: deleted, private and link (never a deleted one).
type controlSets struct{ deleted, private, link []string }

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
		case sc.visibility == visLink:
			s.link = append(s.link, id)
		}
	}
	slices.Sort(s.deleted)
	slices.Sort(s.private)
	slices.Sort(s.link)
	return s
}

func (c *Controls) deleted(sessionID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[sessionID].deleted
}

type controlsKey struct{}

// WithControls puts c on ctx, the way scoped reads and the control tools
// reach it.
func WithControls(ctx context.Context, c *Controls) context.Context {
	return context.WithValue(ctx, controlsKey{}, c)
}

func controlsFrom(ctx context.Context) *Controls {
	c, _ := ctx.Value(controlsKey{}).(*Controls)
	return c
}

// controlArgs is the input both control tools parse.
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
// holds an admin token. Any other session is not found. A wallet reader has
// no members, so to it every session is not found.
func actor(ctx context.Context, db *DB, c *Controls, session string) (auth.Identity, error) {
	id, ok := auth.From(ctx)
	if !ok || c == nil || store.From(ctx) == nil {
		return id, errors.New("index: control tools need an identity, controls and a store on ctx")
	}
	return actorOn(id, db.sessionAuthors(session))
}

// actorOn is actor's decision over one session's authors, the part's session
// authors entry, so the checks are testable on an injected list.
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

// recordKey is the key a control record is appended under: the client's key
// bound to what it asks for, so the key reused for another action is
// another record and only an exact retry is a duplicate.
func recordKey(action controlAction, session string, arg string, client string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{string(action), session, arg, client}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func randomKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

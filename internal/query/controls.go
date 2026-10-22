package query

import (
	"bufio"
	"context"
	"encoding/json"
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

	"github.com/candyflipline/mnemonic/internal/store"
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
)

// controlRecord is one raw line of a control source. The actor is the
// source's author, from its path; Writer is the build that appended it. A
// field this build does not know is one a later build added, and is ignored.
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
	case r.Action == actVisibility && slices.Contains([]visibility{visPrivate, visTeam, visLink}, r.Visibility):
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

// record appends r as author's, stamped now, and applies it. A request key
// already appended changes nothing: its record was applied when appended.
func (c *Controls) record(st *store.Store, author string, r controlRecord) (duplicate bool, err error) {
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

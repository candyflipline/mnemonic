package reader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/candyflipline/mnemonic/arena/route"
	"github.com/candyflipline/mnemonic/arena/trace"
)

const maxBody = 64 << 20

// Cache is a stateless reader's disk cache of packed parts, keyed (tenant, id,
// gen), plus the open parts. A key's bytes never change, so nothing is ever
// invalidated and nothing is evicted.
type Cache struct {
	dir    string
	client *http.Client
	h      *handler
	mu     sync.Mutex
	flight map[string]*flight
}

type flight struct {
	done chan struct{}
	part *trace.Part
	err  error
}

func NewCache(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "*", ".fetch-*"))
	for _, f := range stale {
		os.Remove(f)
	}
	c := &Cache{dir: dir, client: &http.Client{}, flight: map[string]*flight{}}
	c.h = &handler{resolve: c.Get}
	return c, nil
}

func safe(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// Get returns the open part, fetching it through its grant on first use;
// concurrent calls for one key fetch once. A Local part is an error: only the
// keeper holds those.
func (c *Cache) Get(ctx context.Context, tenant string, p route.GrantedPart) (*trace.Part, error) {
	if p.Local {
		return nil, fmt.Errorf("part %s is keeper-local and cannot be read here", p.ID)
	}
	if !safe(tenant) || !safe(p.ID) {
		return nil, fmt.Errorf("bad tenant %q or part id %q", tenant, p.ID)
	}
	key := fmt.Sprintf("%s/%s.g%d", tenant, p.ID, p.Gen)
	c.mu.Lock()
	f, ok := c.flight[key]
	if !ok {
		f = &flight{done: make(chan struct{})}
		c.flight[key] = f
		go func() {
			f.part, f.err = c.load(context.WithoutCancel(ctx), filepath.Join(c.dir, key), p)
			if f.err != nil {
				c.mu.Lock()
				delete(c.flight, key)
				c.mu.Unlock()
			}
			close(f.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.part, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Cache) load(ctx context.Context, path string, p route.GrantedPart) (*trace.Part, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() == p.Size {
		if part, err := trace.OpenPacked(path); err == nil {
			return part, nil
		}
		os.Remove(path)
	}
	if err := c.fetch(ctx, path, p); err != nil {
		return nil, err
	}
	return trace.OpenPacked(path)
}

func (c *Cache) fetch(ctx context.Context, path string, p route.GrantedPart) error {
	if p.URL == "" || p.SHA256 == "" {
		return fmt.Errorf("part %s has no grant URL or no SHA-256", p.ID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch part %s: %s", p.ID, res.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(res.Body, p.Size+1))
	if err != nil {
		tmp.Close()
		return err
	}
	if n != p.Size {
		tmp.Close()
		return fmt.Errorf("part %s: got %d bytes, want %d", p.ID, n, p.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, p.SHA256) {
		tmp.Close()
		return fmt.Errorf("part %s: sha256 %s, want %s", p.ID, got, p.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Prewarm fetches parts, a few at a time, and reports every failure; a part
// that failed is fetched again on its first call.
func (c *Cache) Prewarm(ctx context.Context, tenant string, parts []route.GrantedPart) error {
	sem := make(chan struct{}, 4)
	errs := make([]error, len(parts))
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			_, errs[i] = c.Get(ctx, tenant, p)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Handler serves POST /v1/call: a CallRequest in, a CallResponse out, a text
// body on any error.
func (c *Cache) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/call", func(w http.ResponseWriter, r *http.Request) {
		var req route.CallRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := c.h.call(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

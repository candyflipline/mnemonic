package obj

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type fileStore struct {
	dir    string
	serve  *url.URL
	secret []byte
	ttfb   time.Duration

	requests atomic.Int64
	bytes    atomic.Int64
}

func openFile(u *url.URL) (*fileStore, error) {
	if !filepath.IsAbs(u.Path) {
		return nil, fmt.Errorf("obj: file store %q is not an absolute dir", u.Path)
	}
	if err := os.MkdirAll(u.Path, 0o755); err != nil {
		return nil, err
	}
	f := &fileStore{dir: filepath.Clean(u.Path)}
	q := u.Query()
	if s := q.Get("serve"); s != "" {
		su, err := url.Parse(s)
		if err != nil || su.Scheme != "http" || su.Host == "" {
			return nil, fmt.Errorf("obj: serve=%q is not http://host:port", s)
		}
		f.serve = su
	}
	if s := q.Get("ttfb"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("obj: ttfb=%q is not a duration", s)
		}
		f.ttfb = d
	}
	if s := os.Getenv("ARENA_OBJ_SECRET"); s != "" {
		f.secret = []byte(s)
	} else {
		f.secret = make([]byte, 32)
		if _, err := rand.Read(f.secret); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (f *fileStore) path(k Key) string { return filepath.Join(f.dir, filepath.FromSlash(string(k))) }

func (f *fileStore) Put(ctx context.Context, k Key, r io.Reader, size int64) error {
	return f.write(ctx, k, r, size, false)
}

func (f *fileStore) PutExclusive(ctx context.Context, k Key, r io.Reader, size int64) error {
	return f.write(ctx, k, r, size, true)
}

func (f *fileStore) write(ctx context.Context, k Key, r io.Reader, size int64, exclusive bool) error {
	if err := checkKey(k, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	final := f.path(k)
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r, size))
	if err == nil && n != size {
		err = fmt.Errorf("obj: %s: reader gave %d of %d bytes: %w", k, n, size, io.ErrUnexpectedEOF)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if exclusive {
		if err := os.Link(tmp.Name(), final); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return ErrExists
			}
			return err
		}
	} else if err := os.Rename(tmp.Name(), final); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (f *fileStore) List(ctx context.Context, prefix Key) ([]Entry, error) {
	if err := checkKey(prefix, true); err != nil {
		return nil, err
	}
	root := f.dir
	if i := strings.LastIndex(string(prefix), "/"); i >= 0 {
		root = f.path(prefix[:i])
	}
	var out []Entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		rel, err := filepath.Rel(f.dir, p)
		if err != nil {
			return err
		}
		k := Key(filepath.ToSlash(rel))
		if !strings.HasPrefix(string(k), string(prefix)) {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, Entry{Key: k, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (f *fileStore) Delete(ctx context.Context, keys []Key) error {
	for _, k := range keys {
		if err := checkKey(k, false); err != nil {
			return err
		}
		if err := os.Remove(f.path(k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (f *fileStore) mac(k Key, exp int64) []byte {
	h := hmac.New(sha256.New, f.secret)
	h.Write([]byte(string(k) + "|" + strconv.FormatInt(exp, 10)))
	return h.Sum(nil)
}

func (f *fileStore) PresignGet(k Key, ttl time.Duration) (string, error) {
	if err := checkKey(k, false); err != nil {
		return "", err
	}
	if f.serve == nil {
		return "", errors.New("obj: file store has no serve= address")
	}
	exp := time.Now().Add(ttl).Unix()
	u := *f.serve
	u.Path = "/o/" + string(k)
	u.RawQuery = "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + hex.EncodeToString(f.mac(k, exp))
	return u.String(), nil
}

func (f *fileStore) serveHTTP(ctx context.Context) error {
	if f.serve == nil {
		return errors.New("obj: file store has no serve= address")
	}
	ln, err := net.Listen("tcp", f.serve.Host)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: f}
	stop := context.AfterFunc(ctx, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	})
	defer stop()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (f *fileStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/stats":
		json.NewEncoder(w).Encode(struct {
			Requests int64 `json:"requests"`
			Bytes    int64 `json:"bytes"`
		}{f.requests.Load(), f.bytes.Load()})
	case strings.HasPrefix(r.URL.Path, "/o/") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		f.object(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fileStore) object(w http.ResponseWriter, r *http.Request) {
	k := Key(strings.TrimPrefix(r.URL.Path, "/o/"))
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	sig, err2 := hex.DecodeString(q.Get("sig"))
	if err != nil || err2 != nil || time.Now().Unix() > exp || !hmac.Equal(sig, f.mac(k, exp)) || checkKey(k, false) != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	file, err := os.Open(f.path(k))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if f.ttfb > 0 {
		select {
		case <-time.After(f.ttfb):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method == http.MethodHead {
		http.ServeContent(w, r, "", time.Time{}, file)
		return
	}
	http.ServeContent(&countWriter{ResponseWriter: w, n: &f.bytes}, r, "", time.Time{}, file)
	f.requests.Add(1)
}

type countWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// Command seed writes a mnemonic server data root from transcript files
// through the server's own store handler and the client's redactor, with a
// fixed author, machine and updated_ms so a rerun produces identical bytes.
//
//	seed -src DIR -full ROOT   every source whole
//	seed -src DIR -half ROOT   the first half of each source, cut at a line end
//	seed -src DIR -rest ROOT   append the remainder to a -half root
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	author  = "arena"
	machine = "m1"
	token   = "arena-seed"
	halfMs  = 1767225600000
	fullMs  = halfMs + 60000
	chunk   = 32 << 20
)

type source struct{ dir, harness, sourceName string }

var sources = []source{{"claude", "claude-code", "claude"}, {"codex", "codex", "codex"}}

type manifestBody struct {
	wire.Source
	ZstSize int64 `json:"zst_size"`
}

func main() {
	src := flag.String("src", "", "dir holding claude/ and codex/ jsonl files")
	full := flag.String("full", "", "root to write whole")
	half := flag.String("half", "", "root to write the first half of each source to")
	rest := flag.String("rest", "", "half root to append the remainder to")
	flag.Parse()
	var err error
	switch {
	case *full != "":
		err = run(*src, *full, "full")
	case *half != "":
		err = run(*src, *half, "half")
	case *rest != "":
		err = run(*src, *rest, "rest")
	default:
		err = fmt.Errorf("one of -full, -half, -rest is required")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(srcDir, root, mode string) error {
	red, err := redact.New(nil)
	if err != nil {
		return err
	}
	h := auth.Middleware(map[string]string{auth.Hash(token): author})((&store.Store{DataDir: root}).Handler())
	type job struct {
		s    source
		path string
	}
	var jobs []job
	for _, s := range sources {
		files, err := filepath.Glob(filepath.Join(srcDir, s.dir, "*.jsonl"))
		if err != nil {
			return err
		}
		sort.Strings(files)
		for _, f := range files {
			abs, err := filepath.Abs(f)
			if err != nil {
				return err
			}
			jobs = append(jobs, job{s, abs})
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("-src %q holds no claude/*.jsonl or codex/*.jsonl sources", srcDir)
	}
	errs := make([]error, len(jobs))
	var failedAt atomic.Int64
	failedAt.Store(math.MaxInt64)
	next := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if int64(i) > failedAt.Load() {
					continue
				}
				j := jobs[i]
				if err := seedOne(h, red, root, j.s, j.path, mode); err != nil {
					errs[i] = fmt.Errorf("%s: %w", filepath.Base(j.path), err)
					for cur := failedAt.Load(); int64(i) < cur && !failedAt.CompareAndSwap(cur, int64(i)); cur = failedAt.Load() {
					}
				}
			}
		}()
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	n := len(jobs)
	fmt.Fprintf(os.Stderr, "seed %s: %d sources into %s\n", mode, n, root)
	return nil
}

func seedOne(h http.Handler, red *redact.Redactor, root string, s source, path, mode string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if i := bytes.LastIndexByte(raw, '\n'); i >= 0 {
		raw = raw[:i+1]
	} else {
		return nil
	}
	cut := len(raw)
	if mode != "full" {
		cut = len(raw) / 2
		if i := bytes.IndexByte(raw[cut:], '\n'); i >= 0 {
			cut += i + 1
		}
	}
	var lo, hi int
	ms := int64(fullMs)
	switch mode {
	case "full":
		lo, hi = 0, len(raw)
	case "half":
		lo, hi, ms = 0, cut, halfMs
	case "rest":
		lo, hi = cut, len(raw)
	}
	body, rawEnds := redactLines(red, raw[lo:hi], int64(lo))
	if len(body) == 0 {
		return nil
	}
	headLine := body
	if lo > 0 {
		first, _ := redactLines(red, raw[:bytes.IndexByte(raw, '\n')+1], 0)
		headLine = first
	}
	head := wire.HeadHash(headLine[:bytes.IndexByte(headLine, '\n')+1])
	k := layout.Key{Author: author, Machine: machine, Harness: s.harness, SrcID: wire.SrcID(path)}
	var offset int64
	if lo > 0 {
		m, err := readManifest(k.Manifest(root))
		if err != nil {
			return err
		}
		offset = m.Size
	}
	for len(body) > 0 {
		c := firstChunk(body)
		lines := bytes.Count(c, []byte{'\n'})
		if err := put(h, s, path, k.SrcID, offset, head, c, rawEnds[lines-1]); err != nil {
			return err
		}
		offset += int64(len(c))
		body, rawEnds = body[len(c):], rawEnds[lines:]
	}
	return fixUpdatedMs(k.Manifest(root), ms)
}

func put(h http.Handler, s source, path, srcID string, offset int64, head string, body []byte, rawEnd int64) error {
	q := url.Values{
		"offset":      {strconv.FormatInt(offset, 10)},
		"head_hash":   {head},
		"source_name": {s.sourceName},
		"path":        {path},
		"raw_end":     {strconv.FormatInt(rawEnd, 10)},
	}
	u := wire.RawPathPrefix + machine + "/" + s.harness + "/" + srcID + "?" + q.Encode()
	req := httptest.NewRequest(http.MethodPut, u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("put at %d: %d %s", offset, rec.Code, bytes.TrimSpace(rec.Body.Bytes()))
	}
	return nil
}

func redactLines(red *redact.Redactor, raw []byte, base int64) ([]byte, []int64) {
	body := make([]byte, 0, len(raw))
	var ends []int64
	at := base
	for {
		nl := bytes.IndexByte(raw, '\n')
		if nl < 0 {
			return body, ends
		}
		body = append(body, red.Line(raw[:nl])...)
		body = append(body, '\n')
		at += int64(nl + 1)
		ends = append(ends, at)
		raw = raw[nl+1:]
	}
}

func firstChunk(body []byte) []byte {
	if len(body) <= chunk {
		return body
	}
	if i := bytes.LastIndexByte(body[:chunk], '\n'); i >= 0 {
		return body[:i+1]
	}
	return body[:bytes.IndexByte(body, '\n')+1]
}

func readManifest(path string) (manifestBody, error) {
	var m manifestBody
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func fixUpdatedMs(path string, ms int64) error {
	m, err := readManifest(path)
	if err != nil {
		return err
	}
	m.UpdatedMs = ms
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".seedtmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

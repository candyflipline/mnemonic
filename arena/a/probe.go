package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

const (
	blobSize     = 8 << 20
	killGetP99   = 500 * time.Millisecond
	killErrRate  = 0.01
	opTimeout    = 60 * time.Second
	soakInterval = 200 * time.Millisecond
	raceWriters  = 4
)

type cell struct {
	name string
	run  func(ctx context.Context, i int) (miss bool, err error)
}

type acc struct {
	ds           []time.Duration
	errors, miss int
}

type cellStat struct {
	Name   string  `json:"name"`
	N      int     `json:"n"`
	Errors int     `json:"errors"`
	Misses int     `json:"misses"`
	P50ms  float64 `json:"p50_ms"`
	P99ms  float64 `json:"p99_ms"`
	Maxms  float64 `json:"max_ms"`
}

type verdict struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

type recorder struct {
	order []string
	m     map[string]*acc
}

func (r *recorder) record(ctx context.Context, c cell, i int) {
	if r.m == nil {
		r.m = map[string]*acc{}
	}
	a := r.m[c.name]
	if a == nil {
		a = &acc{}
		r.m[c.name] = a
		r.order = append(r.order, c.name)
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	t0 := time.Now()
	miss, err := c.run(cctx, i)
	d := time.Since(t0)
	a.ds = append(a.ds, d)
	if err != nil {
		a.errors++
	}
	if miss {
		a.miss++
	}
}

func (r *recorder) stats() []cellStat {
	var out []cellStat
	for _, n := range r.order {
		a := r.m[n]
		ds := append([]time.Duration(nil), a.ds...)
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		q := func(p float64) float64 {
			idx := int(p*float64(len(ds))+0.999999) - 1
			return float64(ds[max(idx, 0)]) / float64(time.Millisecond)
		}
		out = append(out, cellStat{n, len(ds), a.errors, a.miss, q(0.5), q(0.99), q(1)})
	}
	return out
}

func find(ss []cellStat, name string) cellStat {
	for _, s := range ss {
		if s.Name == name {
			return s
		}
	}
	return cellStat{Name: name}
}

func clean(ss ...cellStat) bool {
	for _, s := range ss {
		if s.Errors+s.Misses != 0 {
			return false
		}
	}
	return true
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	uri := fs.String("store", "", "obj store URI, file:///dir?serve=http://127.0.0.1:PORT or s3://bucket/prefix?endpoint=HOST&region=R")
	n := fs.Int("n", 30, "samples per cell")
	soak := fs.Duration("soak", 0, "soak duration at about 5 req/s after the cells")
	out := fs.String("out", "", "write the JSON result here")
	fs.Parse(args)
	if *uri == "" || *n < 1 {
		return errors.New("probe: -store is required and -n must be at least 1")
	}
	u, err := url.Parse(*uri)
	if err != nil {
		return err
	}
	st, err := obj.Open(*uri)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if u.Scheme == "file" {
		if err := startServe(ctx, st, u.Query().Get("serve")); err != nil {
			return err
		}
	}
	run := fmt.Sprintf("probe/%d-%04x/", time.Now().Unix(), rand.IntN(1<<16))
	blob := make([]byte, blobSize)
	rng := rand.NewChaCha8([32]byte{1})
	rng.Read(blob)
	if err := st.Put(ctx, obj.Key(run+"blob"), bytes.NewReader(blob), blobSize); err != nil {
		return fmt.Errorf("probe: put blob: %w", err)
	}
	defer func() {
		es, err := st.List(context.Background(), obj.Key(run))
		var ks []obj.Key
		for _, e := range es {
			ks = append(ks, e.Key)
		}
		if err == nil {
			err = st.Delete(context.Background(), ks)
		}
		fmt.Printf("cleanup: %d keys under %s, err=%v\n", len(ks), run, err)
	}()

	cells := probeCells(st, run, blob)
	var rec recorder
	aborted := ""
	for i := 0; i < *n && aborted == ""; i++ {
		for _, c := range cells {
			rec.record(ctx, c, i)
			if i == 0 && strings.HasPrefix(c.name, "putx.") {
				a := rec.m[c.name]
				if a.errors+a.miss > 0 {
					aborted = c.name + " failed on its first sample: conditional create is not usable here, the generation authority moves to a control-plane service (owner call)"
					break
				}
			}
		}
	}
	stats := rec.stats()

	var soakStats []cellStat
	if aborted == "" && *soak > 0 {
		soakStats = runSoak(ctx, cells, *soak, *n)
	}

	vs := judge(stats, soakStats, aborted)
	res := struct {
		Store      string     `json:"store"`
		N          int        `json:"n"`
		Soak       string     `json:"soak"`
		Start      time.Time  `json:"start"`
		Aborted    string     `json:"aborted,omitempty"`
		Cells      []cellStat `json:"cells"`
		SoakCells  []cellStat `json:"soak_cells,omitempty"`
		Assumption []verdict  `json:"assumptions"`
	}{u.Scheme + "://" + u.Host + u.Path, *n, soak.String(), time.Now().UTC(), aborted, stats, soakStats, vs}
	printResult(res.Store, *n, u.Scheme, aborted, stats, soakStats, vs)
	if u.Scheme == "file" {
		if s, err := httpBody(ctx, u.Query().Get("serve")+"/stats"); err == nil {
			fmt.Printf("serve /stats: %s", s)
		}
	}
	if *out != "" {
		b, _ := json.MarshalIndent(res, "", " ")
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	failed := 0
	for _, v := range vs {
		if !v.Pass {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("probe: %d of %d checks failed", failed, len(vs))
	}
	return nil
}

func printResult(store string, n int, scheme, aborted string, stats, soak []cellStat, vs []verdict) {
	fmt.Printf("store %s  n=%d per cell, cells interleaved; p99 is the max below 100 samples\n", store, n)
	if scheme == "file" {
		fmt.Println("NOTE file backend on this machine: not a measurement of S3")
	}
	if aborted != "" {
		fmt.Println("ABORTED:", aborted)
	}
	table := func(title string, ss []cellStat) {
		if len(ss) == 0 {
			return
		}
		fmt.Printf("%-22s %6s %5s %5s %9s %9s\n", title, "n", "err", "miss", "p50ms", "p99ms")
		for _, s := range ss {
			fmt.Printf("%-22s %6d %5d %5d %9.2f %9.2f\n", s.Name, s.N, s.Errors, s.Misses, s.P50ms, s.P99ms)
		}
	}
	table("cell", stats)
	table("soak cell", soak)
	for _, v := range vs {
		r := "PASS"
		if !v.Pass {
			r = "FAIL"
		}
		fmt.Printf("%s  %s  %s\n", r, v.Name, v.Detail)
	}
}

func startServe(ctx context.Context, st obj.Store, serve string) error {
	if serve == "" {
		return errors.New("probe: a file store needs serve=http://127.0.0.1:PORT")
	}
	failed := make(chan error, 1)
	go func() { failed <- obj.Serve(ctx, st) }()
	for i := 0; i < 500; i++ {
		select {
		case err := <-failed:
			return fmt.Errorf("probe: serve: %w", err)
		default:
		}
		if _, err := httpBody(ctx, serve+"/stats"); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("probe: serve never came up")
}

var client = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 90 * time.Second}, Timeout: opTimeout}

func httpBody(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil && resp.StatusCode/100 != 2 {
		err = fmt.Errorf("status %d", resp.StatusCode)
	}
	return b, err
}

func rangeGet(size int) func(st obj.Store, run string, blob []byte) func(context.Context, int) (bool, error) {
	return func(st obj.Store, run string, blob []byte) func(context.Context, int) (bool, error) {
		return func(ctx context.Context, i int) (bool, error) {
			off := rand.IntN(blobSize - size + 1)
			u, err := st.PresignGet(obj.Key(run+"blob"), 10*time.Minute)
			if err != nil {
				return false, err
			}
			req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err != nil {
				return false, err
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+size-1))
			resp, err := client.Do(req)
			if err != nil {
				return false, err
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				return false, err
			}
			if resp.StatusCode != http.StatusPartialContent {
				return false, fmt.Errorf("status %d", resp.StatusCode)
			}
			return !bytes.Equal(b, blob[off:off+size]), nil
		}
	}
}

func probeCells(st obj.Store, run string, blob []byte) []cell {
	small := func(s string) (*strings.Reader, int64) { return strings.NewReader(s), int64(len(s)) }
	created := map[string]int{}
	listHas := func(ctx context.Context, prefix string, want ...string) (bool, error) {
		es, err := st.List(ctx, obj.Key(run+prefix))
		if err != nil {
			return false, err
		}
		if len(es) != created[prefix] {
			return true, nil
		}
		for _, w := range want {
			j := sort.Search(len(es), func(j int) bool { return string(es[j].Key) >= run+w })
			if j == len(es) || string(es[j].Key) != run+w {
				return true, nil
			}
		}
		return false, nil
	}
	key := func(p string, i int) string { return fmt.Sprintf("%s%016x", p, i) }
	return []cell{
		{"putx.create", func(ctx context.Context, i int) (bool, error) {
			r, sz := small("first")
			return false, st.PutExclusive(ctx, obj.Key(run+key("x/", i)), r, sz)
		}},
		{"putx.conflict", func(ctx context.Context, i int) (bool, error) {
			r, sz := small("second")
			err := st.PutExclusive(ctx, obj.Key(run+key("x/", i)), r, sz)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, obj.ErrExists) {
				return false, nil
			}
			return false, err
		}},
		{"gen.create+list", func(ctx context.Context, i int) (bool, error) {
			k := key("g/", i)
			r, sz := small(k)
			if err := st.PutExclusive(ctx, obj.Key(run+k), r, sz); err != nil {
				return false, err
			}
			created["g/"]++
			return listHas(ctx, "g/", k)
		}},
		{"gen.race", func(ctx context.Context, i int) (bool, error) {
			var wg sync.WaitGroup
			var mu sync.Mutex
			wins, other := 0, error(nil)
			for w := 0; w < raceWriters; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, sz := small(fmt.Sprint(w))
					err := st.PutExclusive(ctx, obj.Key(run+key("r/", i)), r, sz)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						wins++
					case !errors.Is(err, obj.ErrExists):
						other = err
					}
				}()
			}
			wg.Wait()
			return other == nil && wins != 1, other
		}},
		{"manifest.put+list", func(ctx context.Context, i int) (bool, error) {
			k := fmt.Sprintf("m/%016x-%016x.json", 1, i)
			r, sz := small("{}")
			if err := st.Put(ctx, obj.Key(run+k), r, sz); err != nil {
				return false, err
			}
			created["m/"]++
			return listHas(ctx, "m/", k)
		}},
		{"get64k.range", rangeGet(64<<10)(st, run, blob)},
		{"get1m.range", rangeGet(1<<20)(st, run, blob)},
		{"get8m.range", rangeGet(8<<20)(st, run, blob)},
		{"delete+list", func(ctx context.Context, i int) (bool, error) {
			k := run + key("d/", i)
			r, sz := small("x")
			if err := st.Put(ctx, obj.Key(k), r, sz); err != nil {
				return false, err
			}
			if err := st.Delete(ctx, []obj.Key{obj.Key(k), obj.Key(k + ".missing")}); err != nil {
				return false, err
			}
			es, err := st.List(ctx, obj.Key(run+"d/"))
			for _, e := range es {
				if string(e.Key) == k {
					return true, err
				}
			}
			return false, err
		}},
	}
}

func runSoak(ctx context.Context, cells []cell, d time.Duration, base int) []cellStat {
	pick := map[string]cell{}
	for _, c := range cells {
		pick[c.name] = c
	}
	rot := []cell{pick["get64k.range"], pick["get64k.range"], pick["manifest.put+list"], pick["get1m.range"]}
	var rec recorder
	tick := time.NewTicker(soakInterval)
	defer tick.Stop()
	end := time.Now().Add(d)
	for i := 0; time.Now().Before(end) && ctx.Err() == nil; i++ {
		<-tick.C
		rec.record(ctx, rot[i%len(rot)], base+i)
		if i%3000 == 2999 {
			fmt.Fprintf(os.Stderr, "soak: %d ops, %s left\n", i+1, time.Until(end).Round(time.Second))
		}
	}
	return rec.stats()
}

func judge(stats, soak []cellStat, aborted string) []verdict {
	g := func(n string) cellStat { return find(stats, n) }
	var vs []verdict
	add := func(name string, pass bool, detail string) { vs = append(vs, verdict{name, pass, detail}) }
	if aborted != "" {
		add("A1 conditional create refuses a second writer", false, aborted)
		return vs
	}
	add("A1 conditional create refuses a second writer", clean(g("putx.create"), g("putx.conflict")), "second PutExclusive on a key returns ErrExists")
	add("A2 create is visible to List on g/ at once", clean(g("gen.create+list")), "create then List shows every claimed gen")
	add("A3 exactly one winner under a race", clean(g("gen.race")), fmt.Sprintf("%d writers per key", raceWriters))
	add("A4 list-after-write is strong on m/", clean(g("manifest.put+list")), "Put then List shows every manifest")
	add("A5 presigned Range reads return exact bytes", clean(g("get64k.range"), g("get1m.range"), g("get8m.range")), "64 KiB, 1 MiB, 8 MiB at random offsets")
	add("A6 Delete is visible to List, a missing key is not an error", clean(g("delete+list")), "")

	p99 := g("get64k.range").P99ms
	src := "cells"
	if s := find(soak, "get64k.range"); s.N > 0 {
		p99, src = s.P99ms, "soak"
	}
	add("K1 64 KiB GET p99 <= 500 ms", p99 <= float64(killGetP99/time.Millisecond), fmt.Sprintf("p99 %.1f ms over %s", p99, src))
	ops, bad := 0, 0
	for _, s := range append(append([]cellStat(nil), stats...), soak...) {
		ops += s.N
		bad += s.Errors
	}
	add("K2 errors and timeouts <= 1%", float64(bad) <= killErrRate*float64(ops), fmt.Sprintf("%d of %d ops", bad, ops))
	miss := 0
	for _, s := range append(append([]cellStat(nil), stats...), soak...) {
		if strings.Contains(s.Name, "list") {
			miss += s.Misses
		}
	}
	add("K3 no list-after-write miss", miss == 0, fmt.Sprintf("%d misses", miss))
	add("K4 presigned PUT usable", true, "not applicable: the keeper uses no presigned PUT")
	return vs
}

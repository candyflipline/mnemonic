// Command check scores an arena candidate binary against the oracle's expected
// results, as the arena CONTRACT's Checking section says.
//
//	check -bin OUT -name NAME             score a candidate
//	check masks -a EXP -b EXP2 -out FILE  record what differs between two oracle runs
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/arena/check/cmp"
	"github.com/klauspost/compress/zstd"
)

var arena string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "masks" {
		fail(mkMasks(os.Args[2:]))
		return
	}
	pass, err := score(os.Args[1:])
	fail(err)
	if !pass {
		os.Exit(2)
	}
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		os.Exit(1)
	}
}

func mkMasks(args []string) error {
	fs := flag.NewFlagSet("masks", flag.ExitOnError)
	set := fs.String("set", "", "set name (golden, hidden)")
	a := fs.String("a", "", "expected.jsonl from oracle run 1")
	b := fs.String("b", "", "expected.jsonl from oracle run 2")
	out := fs.String("out", "", "masks.json to merge into")
	fs.Parse(args)
	ra, err := cmp.ReadResults(*a)
	if err != nil {
		return err
	}
	rb, err := cmp.ReadResults(*b)
	if err != nil {
		return err
	}
	all := cmp.Masks{}
	if b, err := os.ReadFile(*out); err == nil {
		if err := json.Unmarshal(b, &all); err != nil {
			return err
		}
	}
	m := map[int]cmp.Mask{}
	for id, x := range ra {
		y, ok := rb[id]
		if !ok {
			return fmt.Errorf("call %d missing from %s", id, *b)
		}
		var mk cmp.Mask
		var dx, dy any
		cmp.Decode(x.Data, &dx)
		cmp.Decode(y.Data, &dy)
		cmp.DiffPaths("", dx, dy, nil, func(p string) { mk.Data = append(mk.Data, p) })
		lx, ly := strings.Split(x.Text, "\n"), strings.Split(y.Text, "\n")
		if len(lx) != len(ly) {
			return fmt.Errorf("call %d: text line count differs between oracle runs", id)
		}
		for i := range lx {
			if lx[i] != ly[i] {
				mk.TextLines = append(mk.TextLines, i)
			}
		}
		if len(mk.Data) > 0 || len(mk.TextLines) > 0 {
			sort.Strings(mk.Data)
			m[id] = mk
		}
	}
	all[*set] = m
	b2, err := json.MarshalIndent(all, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(*out, append(b2, '\n'), 0o644)
}

type set struct {
	name     string
	calls    []cmp.Call
	expected map[int]cmp.Result
}

type diff struct {
	Set   string   `json:"set"`
	ID    int      `json:"id"`
	Tool  string   `json:"tool"`
	Paths []string `json:"paths"`
}

type tally struct {
	Pass  int    `json:"pass"`
	Fail  int    `json:"fail"`
	Diffs []diff `json:"first_diffs,omitempty"`
}

type parity struct {
	Pass  bool              `json:"pass"`
	Tools map[string]*tally `json:"tools"`
}

func compare(sets []set, ms cmp.Masks, got map[string]map[int]cmp.Result, want func(set, int) (cmp.Result, bool)) parity {
	p := parity{Pass: true, Tools: map[string]*tally{}}
	for _, s := range sets {
		for _, c := range s.calls {
			t := p.Tools[c.Tool]
			if t == nil {
				t = &tally{}
				p.Tools[c.Tool] = t
			}
			var paths []string
			e, ok := want(s, c.ID)
			g, gok := got[s.name][c.ID]
			switch {
			case !ok:
				paths = append(paths, "baseline:missing")
			case !gok:
				paths = append(paths, "result:missing")
			default:
				paths = cmp.Diff(e, g, ms[s.name][c.ID])
			}
			if len(paths) == 0 {
				t.Pass++
				continue
			}
			t.Fail++
			p.Pass = false
			if len(t.Diffs) < 3 {
				if len(paths) > 8 {
					paths = append(paths[:8], fmt.Sprintf("... %d more", len(paths)-8))
				}
				t.Diffs = append(t.Diffs, diff{Set: s.name, ID: c.ID, Tool: c.Tool, Paths: paths})
			}
		}
	}
	return p
}

// run is one ingest's measurements.
type run struct {
	WallMs  int64 `json:"wall_ms"`
	PeakRSS int64 `json:"peak_rss_bytes"`
}

var rssRe = regexp.MustCompile(`(\d+)\s+maximum resident set size`)

var timeReportRe = regexp.MustCompile(`\n\s+[\d.]+\s+real\s`)

func ingest(bin, root, store string) (run, error) {
	cmd := exec.Command("/usr/bin/time", "-l", bin, "ingest", "-root", root, "-store", store)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	start := time.Now()
	err := cmd.Run()
	r := run{WallMs: time.Since(start).Milliseconds()}
	if m := rssRe.FindSubmatch(stderr.Bytes()); m != nil {
		r.PeakRSS, _ = strconv.ParseInt(string(m[1]), 10, 64)
	}
	if err != nil {
		msg := stderr.String()
		if loc := timeReportRe.FindStringIndex(msg); loc != nil {
			msg = msg[:loc[0]]
		}
		return r, fmt.Errorf("ingest %s: %v: %s", store, err, tail(msg))
	}
	return r, nil
}

func tail(s string) string {
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

type batchOut struct {
	results map[string]map[int]cmp.Result
	idleRSS int64
}

func batch(bin, root, store string, sets []set) (batchOut, error) {
	out := batchOut{results: map[string]map[int]cmp.Result{}}
	cmd := exec.Command(bin, "batch", "-root", root, "-store", store)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return out, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return out, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return out, err
	}
	defer cmd.Process.Kill()
	rd := bufio.NewReaderSize(stdout, 1<<20)
	for _, s := range sets {
		out.results[s.name] = map[int]cmp.Result{}
		for _, c := range s.calls {
			b, _ := json.Marshal(c)
			if _, err := stdin.Write(append(b, '\n')); err != nil {
				return out, fmt.Errorf("batch write: %v: %s", err, tail(stderr.String()))
			}
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return out, fmt.Errorf("batch read call %d: %v: %s", c.ID, err, tail(stderr.String()))
			}
			var r cmp.Result
			if err := json.Unmarshal(line, &r); err != nil {
				return out, fmt.Errorf("batch call %d: bad line: %v", c.ID, err)
			}
			if r.ID != c.ID {
				return out, fmt.Errorf("batch answered id %d for call %d", r.ID, c.ID)
			}
			out.results[s.name][c.ID] = r
		}
	}
	time.Sleep(time.Second)
	if b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(cmd.Process.Pid)).Output(); err == nil {
		kb, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		out.idleRSS = kb * 1024
	}
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	return out, nil
}

func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func percentiles(sets []set, got map[string]map[int]cmp.Result) map[string]map[string]int64 {
	by := map[string][]int64{}
	for _, s := range sets {
		for _, c := range s.calls {
			if r, ok := got[s.name][c.ID]; ok {
				by[c.Tool] = append(by[c.Tool], r.US)
			}
		}
	}
	out := map[string]map[string]int64{}
	for tool, us := range by {
		sort.Slice(us, func(i, j int) bool { return us[i] < us[j] })
		q := func(f float64) int64 { return us[min(len(us)-1, int(math.Ceil(f*float64(len(us))))-1)] }
		out[tool] = map[string]int64{"p50": q(0.5), "p99": q(0.99), "n": int64(len(us))}
	}
	return out
}

func nonStdDeps(bin string) int {
	b, err := exec.Command("go", "version", "-m", bin).Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "dep" {
			n++
		}
	}
	return n
}

type crash struct {
	Seed         int64   `json:"seed"`
	KillAfterMs  []int64 `json:"kill_after_ms"`
	ExitedBefore int     `json:"ingests_exited_before_kill"`
	Final        run     `json:"final_ingest"`
	Parity       parity  `json:"parity"`
	Error        string  `json:"error,omitempty"`
}

type card struct {
	Name           string                      `json:"name"`
	Bin            string                      `json:"bin"`
	At             string                      `json:"at"`
	Pass           bool                        `json:"pass"`
	Error          string                      `json:"error,omitempty"`
	Public         *parity                     `json:"public,omitempty"`
	Hidden         *parity                     `json:"hidden,omitempty"`
	Incremental    *parity                     `json:"incremental,omitempty"`
	IncrementalErr string                      `json:"incremental_error,omitempty"`
	Crash          *crash                      `json:"crash,omitempty"`
	AppendCrash    *appendCrash                `json:"append_crash,omitempty"`
	Ingest         run                         `json:"clean_ingest"`
	StoreBytes     int64                       `json:"store_bytes"`
	BatchIdleRSS   int64                       `json:"batch_idle_rss_bytes"`
	LatencyUS      map[string]map[string]int64 `json:"latency_us"`
	BinaryBytes    int64                       `json:"binary_bytes"`
	NonStdDeps     int                         `json:"non_std_deps"`
}

func loadSet(name, dir string) (set, error) {
	cs, err := cmp.ReadCalls(filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		return set{}, err
	}
	exp, err := cmp.ReadResults(filepath.Join(dir, "expected.jsonl"))
	if err != nil {
		return set{}, err
	}
	return set{name: name, calls: cs, expected: exp}, nil
}

func score(args []string) (bool, error) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	bin := fs.String("bin", "", "candidate binary")
	name := fs.String("name", "", "candidate name")
	arenaDir := fs.String("arena", os.Getenv("ARENA"), "arena fixture dir (default $ARENA)")
	src := fs.String("src", "", "transcripts the seed reads (default <arena>/../sample2)")
	seedBin := fs.String("seed", "", "seed binary (default <arena>/check/seed)")
	crashSeed := fs.Int64("crash-seed", time.Now().UnixNano(), "rng seed for kill points")
	skip := fs.String("skip", "", "comma list of phases to skip: incremental,crash,appendcrash")
	fs.Parse(args)
	if *bin == "" || *name == "" {
		return false, fmt.Errorf("-bin and -name are required")
	}
	if *arenaDir == "" {
		return false, fmt.Errorf("-arena or $ARENA is required")
	}
	arena = *arenaDir
	if *src == "" {
		*src = filepath.Join(arena, "..", "sample2")
	}
	if *seedBin == "" {
		*seedBin = filepath.Join(arena, "check", "seed")
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		return false, err
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	work := filepath.Join(arena, "check", "work", *name+"-"+ts)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return false, err
	}
	defer os.RemoveAll(work)
	c := card{Name: *name, Bin: absBin, At: ts, Pass: true}
	if info, err := os.Stat(absBin); err == nil {
		c.BinaryBytes = info.Size()
	}
	c.NonStdDeps = nonStdDeps(absBin)
	err = scoreInto(&c, absBin, work, *src, *seedBin, *crashSeed, *skip)
	if err != nil {
		c.Pass, c.Error = false, err.Error()
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	os.MkdirAll(filepath.Join(arena, "runs"), 0o755)
	out := filepath.Join(arena, "runs", *name+"-"+ts+".json")
	if werr := os.WriteFile(out, append(b, '\n'), 0o644); werr != nil {
		return false, werr
	}
	fmt.Println(string(b))
	fmt.Fprintln(os.Stderr, "scorecard:", out)
	return c.Pass, nil
}

func scoreInto(c *card, bin, work, src, seedBin string, crashSeed int64, skip string) error {
	pub, err := loadSet("golden", filepath.Join(arena, "golden"))
	if err != nil {
		return err
	}
	hid, err := loadSet("hidden", filepath.Join(arena, "hidden"))
	if err != nil {
		return err
	}
	sets := []set{pub, hid}
	ms, err := cmp.ReadMasks(filepath.Join(arena, "check", "masks.json"))
	if err != nil {
		return err
	}
	raw := filepath.Join(arena, "raw")

	clean := filepath.Join(work, "clean")
	if c.Ingest, err = ingest(bin, raw, clean); err != nil {
		return err
	}
	c.StoreBytes = dirBytes(clean)
	got, err := batch(bin, raw, clean, sets)
	if err != nil {
		return err
	}
	c.BatchIdleRSS = got.idleRSS
	c.LatencyUS = percentiles(sets, got.results)
	vsExpected := func(s set, id int) (cmp.Result, bool) { r, ok := s.expected[id]; return r, ok }
	p := compare([]set{pub}, ms, got.results, vsExpected)
	h := compare([]set{hid}, ms, got.results, vsExpected)
	c.Public, c.Hidden = &p, &h
	c.Pass = p.Pass && h.Pass
	vsClean := func(s set, id int) (cmp.Result, bool) { r, ok := got.results[s.name][id]; return r, ok }

	if !strings.Contains(skip, "incremental") {
		ip, err := incremental(bin, work, src, seedBin, sets, ms, vsClean)
		if err != nil {
			c.IncrementalErr = err.Error()
			c.Pass = false
		} else {
			c.Incremental = &ip
			c.Pass = c.Pass && ip.Pass
		}
	}
	if !strings.Contains(skip, "crash") {
		cr := crashRun(bin, raw, work, c.Ingest.WallMs, crashSeed, sets, ms, vsClean)
		c.Crash = &cr
		c.Pass = c.Pass && cr.Error == "" && cr.Parity.Pass
	}
	if !strings.Contains(skip, "appendcrash") {
		ac := appendCrashRun(bin, raw, work, crashSeed, sets, ms, vsClean)
		c.AppendCrash = &ac
		c.Pass = c.Pass && ac.Error == "" && ac.Parity.Pass
	}
	return nil
}

func incremental(bin, work, src, seedBin string, sets []set, ms cmp.Masks, want func(set, int) (cmp.Result, bool)) (parity, error) {
	root := filepath.Join(work, "incr-root")
	store := filepath.Join(work, "incr-store")
	if out, err := exec.Command(seedBin, "-src", src, "-half", root).CombinedOutput(); err != nil {
		return parity{}, fmt.Errorf("seed -half: %v: %s", err, tail(string(out)))
	}
	if _, err := ingest(bin, root, store); err != nil {
		return parity{}, err
	}
	if out, err := exec.Command(seedBin, "-src", src, "-rest", root).CombinedOutput(); err != nil {
		return parity{}, fmt.Errorf("seed -rest: %v: %s", err, tail(string(out)))
	}
	if _, err := ingest(bin, root, store); err != nil {
		return parity{}, err
	}
	got, err := batch(bin, root, store, sets)
	if err != nil {
		return parity{}, err
	}
	return compare(sets, ms, got.results, want), nil
}

// crashRun lands three SIGKILLs on ingests into one store, each before that
// ingest exits, then re-runs ingest to the end. An ingest that exits before
// its kill leaves a complete store, so the store is dropped and the kills
// start over, at most eight attempts.
func crashRun(bin, raw, work string, cleanMs, seed int64, sets []set, ms cmp.Masks, want func(set, int) (cmp.Result, bool)) crash {
	cr := crash{Seed: seed}
	r := rand.New(rand.NewSource(seed))
	store := filepath.Join(work, "crash-store")
	landed := 0
	for attempt := 0; landed < 3 && attempt < 8; attempt++ {
		window := cleanMs * 6 / 10
		if landed > 0 {
			window = cleanMs * 3 / 10
		}
		after := r.Int63n(max(window, 1)) + 1
		killed, _, err := killAfter(bin, raw, store, time.Duration(after)*time.Millisecond)
		if err != nil {
			cr.Error = err.Error()
			return cr
		}
		if !killed {
			cr.ExitedBefore++
			landed = 0
			cr.KillAfterMs = nil
			os.RemoveAll(store)
			continue
		}
		landed++
		cr.KillAfterMs = append(cr.KillAfterMs, after)
	}
	if landed < 3 {
		cr.Error = fmt.Sprintf("only %d kills landed before an ingest exited", landed)
		return cr
	}
	var err error
	if cr.Final, err = ingest(bin, raw, store); err != nil {
		cr.Error = err.Error()
		return cr
	}
	got, err := batch(bin, raw, store, sets)
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	cr.Parity = compare(sets, ms, got.results, want)
	return cr
}

// killAfter runs an ingest and SIGKILLs it after d unless it exits first. It
// reports whether the kill landed and how long the ingest ran.
func killAfter(bin, root, store string, d time.Duration) (bool, time.Duration, error) {
	cmd := exec.Command(bin, "ingest", "-root", root, "-store", store)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return false, 0, err
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
		return false, time.Since(start), nil
	case <-time.After(d):
		cmd.Process.Signal(syscall.SIGKILL)
		<-done
		return true, time.Since(start), nil
	}
}

type appendCrash struct {
	Seed             int64   `json:"seed"`
	Steps            int     `json:"steps"`
	KillsLanded      int     `json:"kills_landed"`
	KillAfterMs      []int64 `json:"kill_after_ms"`
	ExitedBefore     int     `json:"ingests_exited_before_kill"`
	DeltaPartsAtKill []int   `json:"delta_parts_at_kill"`
	TornLogsAtKill   []int   `json:"torn_delta_logs_at_kill"`
	Final            run     `json:"final_ingest"`
	Parity           parity  `json:"parity"`
	Error            string  `json:"error,omitempty"`
}

const (
	appendSteps    = 12
	appendFirstPct = 70
	appendMinKills = 3
)

// appendCrashRun grows a copy of raw in appendSteps slices, the first
// appendFirstPct percent of every source and then equal steps to the whole,
// ingesting between slices. Each ingest after the first two is SIGKILLed
// somewhere inside it, so the delta path of a hot component is killed while it
// appends. The last slice is ingested to the end and compared with the clean
// run.
func appendCrashRun(bin, raw, work string, seed int64, sets []set, ms cmp.Masks, want func(set, int) (cmp.Result, bool)) appendCrash {
	ac := appendCrash{Seed: seed, Steps: appendSteps}
	r := rand.New(rand.NewSource(seed))
	root, store := filepath.Join(work, "append-root"), filepath.Join(work, "append-store")
	g, err := newGrowth(raw, root, appendSteps)
	if err != nil {
		ac.Error = err.Error()
		return ac
	}
	for k := 0; k < 2; k++ {
		if err := g.grow(k); err != nil {
			ac.Error = err.Error()
			return ac
		}
		first, err := ingest(bin, root, store)
		if err != nil {
			ac.Error = err.Error()
			return ac
		}
		if k == 1 {
			g.estimate = time.Duration(first.WallMs) * time.Millisecond
		}
	}
	for k := 2; k < appendSteps; k++ {
		if err := g.grow(k); err != nil {
			ac.Error = err.Error()
			return ac
		}
		after := time.Duration(r.Int63n(max(g.estimate.Milliseconds(), 1))+1) * time.Millisecond
		killed, ran, err := killAfter(bin, root, store, after)
		if err != nil {
			ac.Error = err.Error()
			return ac
		}
		if !killed {
			ac.ExitedBefore++
			g.estimate = ran
			continue
		}
		parts, torn := deltaCensus(store)
		ac.KillsLanded++
		ac.KillAfterMs = append(ac.KillAfterMs, after.Milliseconds())
		ac.DeltaPartsAtKill = append(ac.DeltaPartsAtKill, parts)
		ac.TornLogsAtKill = append(ac.TornLogsAtKill, torn)
	}
	if err := g.grow(appendSteps); err != nil {
		ac.Error = err.Error()
		return ac
	}
	if ac.Final, err = ingest(bin, root, store); err != nil {
		ac.Error = err.Error()
		return ac
	}
	live := 0
	for _, n := range ac.DeltaPartsAtKill {
		if n > 0 {
			live++
		}
	}
	switch {
	case ac.KillsLanded < appendMinKills:
		ac.Error = fmt.Sprintf("only %d kills landed before an ingest exited, need %d", ac.KillsLanded, appendMinKills)
		return ac
	case live < appendMinKills:
		ac.Error = fmt.Sprintf("only %d kills found a committed delta.json in the store, need %d: the delta path was not live", live, appendMinKills)
		return ac
	}
	got, err := batch(bin, root, store, sets)
	if err != nil {
		ac.Error = err.Error()
		return ac
	}
	ac.Parity = compare(sets, ms, got.results, want)
	return ac
}

// growth is a copy of a raw root that is written in slices: slice 0 is the
// first appendFirstPct percent of every source and slice steps is the whole.
type growth struct {
	root     string
	srcs     []slicedSource
	steps    int
	estimate time.Duration
}

type slicedSource struct {
	rel    string
	man    map[string]json.RawMessage
	frames [][]byte
	ends   []int64
	zst    int64
}

func newGrowth(raw, root string, steps int) (*growth, error) {
	g := &growth{root: root, steps: steps}
	paths, err := filepath.Glob(filepath.Join(raw, "raw", "author=*", "machine=*", "harness=*", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	for _, p := range paths {
		rel, err := filepath.Rel(raw, p)
		if err != nil {
			return nil, err
		}
		s := slicedSource{rel: rel}
		mb, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(mb, &s.man); err != nil {
			return nil, fmt.Errorf("%s: %v", rel, err)
		}
		zb, err := os.ReadFile(strings.TrimSuffix(p, ".json") + ".jsonl.zst")
		if err != nil {
			return nil, err
		}
		body, err := dec.DecodeAll(zb, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", rel, err)
		}
		var size int64
		if err := json.Unmarshal(s.man["size"], &size); err != nil || size != int64(len(body)) || len(body) == 0 || body[len(body)-1] != '\n' {
			return nil, fmt.Errorf("%s: manifest size %s does not match the %d raw bytes", rel, s.man["size"], len(body))
		}
		s.ends = make([]int64, steps+1)
		for k := 0; k < steps; k++ {
			at := int64(len(body)) * int64(appendFirstPct*steps+(100-appendFirstPct)*k) / int64(100*steps)
			s.ends[k] = at + int64(bytes.IndexByte(body[at:], '\n')) + 1
		}
		s.ends[steps] = int64(len(body))
		s.frames = make([][]byte, steps+1)
		for k := 0; k <= steps; k++ {
			prev := int64(0)
			if k > 0 {
				prev = s.ends[k-1]
			}
			if s.ends[k] > prev {
				s.frames[k] = enc.EncodeAll(body[prev:s.ends[k]], nil)
			}
		}
		g.srcs = append(g.srcs, s)
	}
	return g, nil
}

func (g *growth) grow(k int) error {
	for i := range g.srcs {
		s := &g.srcs[i]
		if len(s.frames[k]) == 0 {
			continue
		}
		manPath := filepath.Join(g.root, s.rel)
		zstPath := strings.TrimSuffix(manPath, ".json") + ".jsonl.zst"
		if err := os.MkdirAll(filepath.Dir(manPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(zstPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		_, err = f.Write(s.frames[k])
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		s.zst += int64(len(s.frames[k]))
		m := cloneManifest(s.man)
		num := func(v int64) json.RawMessage { return json.RawMessage(strconv.FormatInt(v, 10)) }
		m["zst_size"] = num(s.zst)
		if k < g.steps {
			m["size"], m["raw_end"] = num(s.ends[k]), num(s.ends[k])
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := os.WriteFile(manPath+".tmp", append(b, '\n'), 0o644); err != nil {
			return err
		}
		if err := os.Rename(manPath+".tmp", manPath); err != nil {
			return err
		}
	}
	return nil
}

func cloneManifest(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// deltaCensus counts the committed delta.json files in the store and the delta
// logs that hold bytes past what the newest committed delta.json of their
// image names, the tail a killed append leaves.
func deltaCensus(store string) (parts, torn int) {
	type committed struct{ light, heavy, rows int64 }
	latest := map[string]committed{}
	paths, _ := filepath.Glob(filepath.Join(store, "parts", "*", "delta.json"))
	for _, p := range paths {
		if strings.HasSuffix(filepath.Dir(p), ".tmp") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var d struct {
			Image    string `json:"image"`
			LightLen int64  `json:"light_len"`
			HeavyLen int64  `json:"heavy_len"`
			Rows     int64  `json:"rows"`
		}
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		parts++
		if cur, ok := latest[d.Image]; !ok || d.Rows > cur.rows {
			latest[d.Image] = committed{d.LightLen, d.HeavyLen, d.Rows}
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(store, "deltas", "*"))
	for _, dd := range dirs {
		c := latest[filepath.Base(dd)]
		size := func(name string) int64 {
			if fi, err := os.Stat(filepath.Join(dd, name)); err == nil {
				return fi.Size()
			}
			return 0
		}
		if size("light") > c.light || size("heavy") > c.heavy || size("src") > 2*c.rows {
			torn++
		}
	}
	return parts, torn
}

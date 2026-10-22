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
)

const arena = "/Users/user/.claude/jobs/47d8265e/tmp/arena"

type call struct {
	ID   int             `json:"id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type result struct {
	ID    int             `json:"id"`
	Data  json.RawMessage `json:"data"`
	Text  string          `json:"text"`
	Error string          `json:"error"`
	US    int64           `json:"us"`
}

// mask is what one call may differ in between two runs of the oracle: data
// paths with indices, and text line numbers.
type mask struct {
	Data      []string `json:"data,omitempty"`
	TextLines []int    `json:"text_lines,omitempty"`
}

type masks map[string]map[int]mask

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
	ra, err := readResults(*a)
	if err != nil {
		return err
	}
	rb, err := readResults(*b)
	if err != nil {
		return err
	}
	all := masks{}
	if b, err := os.ReadFile(*out); err == nil {
		if err := json.Unmarshal(b, &all); err != nil {
			return err
		}
	}
	m := map[int]mask{}
	for id, x := range ra {
		y, ok := rb[id]
		if !ok {
			return fmt.Errorf("call %d missing from %s", id, *b)
		}
		var mk mask
		var dx, dy any
		decode(x.Data, &dx)
		decode(y.Data, &dy)
		diffPaths("", dx, dy, nil, func(p string) { mk.Data = append(mk.Data, p) })
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

func decode(raw json.RawMessage, v *any) {
	if len(raw) == 0 {
		*v = nil
		return
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.Decode(v)
}

// diffPaths reports each path where a and b differ. A masked path matches
// anything; numbers match within a relative 1e-9 unless both are integers.
func diffPaths(p string, a, b any, masked map[string]bool, report func(string)) {
	if masked[p] {
		return
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			report(p + ":type")
			return
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			va, oka := x[k]
			vb, okb := y[k]
			switch {
			case !oka:
				report(p + "." + k + ":extra")
			case !okb:
				report(p + "." + k + ":missing")
			default:
				diffPaths(p+"."+k, va, vb, masked, report)
			}
		}
	case []any:
		y, ok := b.([]any)
		if !ok {
			report(p + ":type")
			return
		}
		if len(x) != len(y) {
			report(fmt.Sprintf("%s:len %d!=%d", p, len(x), len(y)))
		}
		for i := range min(len(x), len(y)) {
			diffPaths(p+"["+strconv.Itoa(i)+"]", x[i], y[i], masked, report)
		}
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			report(p + ":type")
			return
		}
		if !numEqual(x, y) {
			report(p)
		}
	default:
		if a != b {
			report(p)
		}
	}
}

func numEqual(a, b json.Number) bool {
	if a == b {
		return true
	}
	ia, ea := a.Int64()
	ib, eb := b.Int64()
	if ea == nil && eb == nil {
		return ia == ib
	}
	fa, err1 := a.Float64()
	fb, err2 := b.Float64()
	if err1 != nil || err2 != nil {
		return false
	}
	if fa == fb {
		return true
	}
	return math.Abs(fa-fb) <= 1e-9*math.Max(math.Abs(fa), math.Abs(fb))
}

func readCalls(path string) ([]call, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cs []call
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var c call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, sc.Err()
}

func readResults(path string) (map[int]result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rs := map[int]result{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		rs[r.ID] = r
	}
	return rs, sc.Err()
}

type set struct {
	name     string
	calls    []call
	expected map[int]result
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

func compare(sets []set, ms masks, got map[string]map[int]result, want func(set, int) (result, bool)) parity {
	p := parity{Pass: true, Tools: map[string]*tally{}}
	for _, s := range sets {
		for _, c := range s.calls {
			t := p.Tools[c.Tool]
			if t == nil {
				t = &tally{}
				p.Tools[c.Tool] = t
			}
			var paths []string
			report := func(x string) { paths = append(paths, x) }
			e, ok := want(s, c.ID)
			g, gok := got[s.name][c.ID]
			switch {
			case !ok:
				paths = append(paths, "baseline:missing")
			case !gok:
				paths = append(paths, "result:missing")
			default:
				mk := ms[s.name][c.ID]
				masked := map[string]bool{}
				for _, x := range mk.Data {
					masked[x] = true
				}
				var de, dg any
				decode(e.Data, &de)
				decode(g.Data, &dg)
				diffPaths("data", de, dg, prefixed(masked), report)
				textDiff(e.Text, g.Text, mk.TextLines, report)
				if (e.Error == "") != (g.Error == "") {
					report("error")
				}
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

func prefixed(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out["data"+k] = true
	}
	return out
}

func textDiff(want, got string, maskedLines []int, report func(string)) {
	if want == got {
		return
	}
	lw, lg := strings.Split(want, "\n"), strings.Split(got, "\n")
	if len(lw) != len(lg) {
		report(fmt.Sprintf("text:lines %d!=%d", len(lw), len(lg)))
		return
	}
	skip := map[int]bool{}
	for _, i := range maskedLines {
		skip[i] = true
	}
	for i := range lw {
		if lw[i] != lg[i] && !skip[i] {
			report("text:line " + strconv.Itoa(i))
		}
	}
}

// run is one ingest's measurements.
type run struct {
	WallMs  int64 `json:"wall_ms"`
	PeakRSS int64 `json:"peak_rss_bytes"`
}

var rssRe = regexp.MustCompile(`(\d+)\s+maximum resident set size`)

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
		return r, fmt.Errorf("ingest %s: %v: %s", store, err, tail(stderr.String()))
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
	results map[string]map[int]result
	idleRSS int64
}

func batch(bin, root, store string, sets []set) (batchOut, error) {
	out := batchOut{results: map[string]map[int]result{}}
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
		out.results[s.name] = map[int]result{}
		for _, c := range s.calls {
			b, _ := json.Marshal(c)
			if _, err := stdin.Write(append(b, '\n')); err != nil {
				return out, fmt.Errorf("batch write: %v: %s", err, tail(stderr.String()))
			}
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return out, fmt.Errorf("batch read call %d: %v: %s", c.ID, err, tail(stderr.String()))
			}
			var r result
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

func percentiles(sets []set, got map[string]map[int]result) map[string]map[string]int64 {
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
	Ingest         run                         `json:"clean_ingest"`
	StoreBytes     int64                       `json:"store_bytes"`
	BatchIdleRSS   int64                       `json:"batch_idle_rss_bytes"`
	LatencyUS      map[string]map[string]int64 `json:"latency_us"`
	BinaryBytes    int64                       `json:"binary_bytes"`
	NonStdDeps     int                         `json:"non_std_deps"`
}

func loadSet(name, dir string) (set, error) {
	cs, err := readCalls(filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		return set{}, err
	}
	exp, err := readResults(filepath.Join(dir, "expected.jsonl"))
	if err != nil {
		return set{}, err
	}
	return set{name: name, calls: cs, expected: exp}, nil
}

func score(args []string) (bool, error) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	bin := fs.String("bin", "", "candidate binary")
	name := fs.String("name", "", "candidate name")
	src := fs.String("src", filepath.Join(arena, "..", "sample2"), "transcripts the seed reads")
	seedBin := fs.String("seed", filepath.Join(arena, "check", "seed"), "seed binary")
	crashSeed := fs.Int64("crash-seed", time.Now().UnixNano(), "rng seed for kill points")
	skip := fs.String("skip", "", "comma list of phases to skip: incremental,crash")
	fs.Parse(args)
	if *bin == "" || *name == "" {
		return false, fmt.Errorf("-bin and -name are required")
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
	ms := masks{}
	mb, err := os.ReadFile(filepath.Join(arena, "check", "masks.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(mb, &ms); err != nil {
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
	vsExpected := func(s set, id int) (result, bool) { r, ok := s.expected[id]; return r, ok }
	p := compare([]set{pub}, ms, got.results, vsExpected)
	h := compare([]set{hid}, ms, got.results, vsExpected)
	c.Public, c.Hidden = &p, &h
	c.Pass = p.Pass && h.Pass
	vsClean := func(s set, id int) (result, bool) { r, ok := got.results[s.name][id]; return r, ok }

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
	return nil
}

func incremental(bin, work, src, seedBin string, sets []set, ms masks, want func(set, int) (result, bool)) (parity, error) {
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
func crashRun(bin, raw, work string, cleanMs, seed int64, sets []set, ms masks, want func(set, int) (result, bool)) crash {
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
		cmd := exec.Command(bin, "ingest", "-root", raw, "-store", store)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			cr.Error = err.Error()
			return cr
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
			cr.ExitedBefore++
			landed = 0
			cr.KillAfterMs = nil
			os.RemoveAll(store)
			continue
		case <-time.After(time.Duration(after) * time.Millisecond):
			cmd.Process.Signal(syscall.SIGKILL)
			<-done
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

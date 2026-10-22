// Command e2e drives a keeper's /v1/call with the fixture's calls and diffs
// the answers against the expected results with arena/check/cmp.
//
//	e2e drive    -keeper URL -arena DIR -rung whole|partial   send every call, diff, print one line per pass
//	e2e ingest   -keeper URL                                  one ingest and commit cycle
//	e2e manifest -dir DIR -tenant T [-gen N]                  latest manifest, and whether every part it names exists
//	e2e tools    -arena DIR                                   the tools the fixture calls, one per line
//	e2e stats    -url URL                                     the file store's request and byte counters
//	e2e ports    N                                            N free 127.0.0.1 ports
//	e2e wait     -addr HOST:PORT -timeout D                   until the address accepts connections
//	e2e unpack   -raw DIR -src DIR                            the claude and codex sources of a raw tree as seed's SRC layout
//
// E2E_TIMEOUT (a Go duration, default 120s) bounds every request to the keeper, an ingest included.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/arena/check/cmp"
	"github.com/candyflipline/mnemonic/arena/keeper"
)

var sets = []string{"golden", "hidden"}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: e2e drive|ingest|manifest|tools|stats|ports|wait|unpack ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "drive":
		err = drive(os.Args[2:])
	case "ingest":
		err = ingest(os.Args[2:])
	case "manifest":
		err = manifest(os.Args[2:])
	case "tools":
		err = tools(os.Args[2:])
	case "stats":
		err = stats(os.Args[2:])
	case "ports":
		err = ports(os.Args[2:])
	case "wait":
		err = wait(os.Args[2:])
	case "unpack":
		err = unpack(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %s", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
}

type fixtureSet struct {
	name  string
	calls []cmp.Call
	want  map[int]cmp.Result
}

func loadSets(arena, baseline string) ([]fixtureSet, error) {
	var out []fixtureSet
	for _, name := range sets {
		cs, err := cmp.ReadCalls(filepath.Join(arena, name, "calls.jsonl"))
		if err != nil {
			return nil, err
		}
		wantPath := filepath.Join(arena, name, "expected.jsonl")
		if baseline != "" {
			wantPath = filepath.Join(baseline, name+".jsonl")
		}
		want, err := cmp.ReadResults(wantPath)
		if err != nil {
			return nil, err
		}
		out = append(out, fixtureSet{name: name, calls: cs, want: want})
	}
	return out, nil
}

type keeperReply struct {
	Data  json.RawMessage `json:"data"`
	Text  string          `json:"text"`
	Error string          `json:"error"`
}

type callBody struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	Rung string          `json:"rung"`
}

var client = &http.Client{Timeout: requestTimeout()}

func requestTimeout() time.Duration {
	if v := os.Getenv("E2E_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		fmt.Fprintln(os.Stderr, "e2e: E2E_TIMEOUT is not a duration:", v)
		os.Exit(2)
	}
	return 120 * time.Second
}

func post(url string, body any, out any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("status %d: body is not JSON", resp.StatusCode)
		}
	}
	return resp.StatusCode, nil
}

type counters struct{ Requests, Bytes int64 }

func readStats(url string) (counters, error) {
	resp, err := client.Get(url)
	if err != nil {
		return counters{}, err
	}
	defer resp.Body.Close()
	var c struct {
		Requests int64 `json:"requests"`
		Bytes    int64 `json:"bytes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return counters{}, err
	}
	return counters{c.Requests, c.Bytes}, nil
}

func drive(args []string) error {
	fs := flag.NewFlagSet("drive", flag.ExitOnError)
	keeperURL := fs.String("keeper", "", "keeper base URL")
	arena := fs.String("arena", os.Getenv("ARENA"), "fixture dir (default $ARENA)")
	rung := fs.String("rung", "partial", "whole or partial")
	baseline := fs.String("baseline", "", "dir of <set>.jsonl results to compare to instead of the fixture's expected")
	save := fs.String("save", "", "dir to write the first pass's results to, as <set>.jsonl")
	only := fs.String("tool", "", "send only this tool's calls")
	passes := fs.Int("passes", 1, "send the calls this many times")
	statsURL := fs.String("stats", "", "file store /stats URL; each pass line carries the requests and bytes it fetched")
	killPID := fs.Int("kill-pid", 0, "SIGKILL this pid before call -kill-at of the first pass")
	killAt := fs.Int("kill-at", 0, "0-based index into the sent calls")
	fs.Parse(args)
	if *keeperURL == "" || *arena == "" {
		return fmt.Errorf("-keeper and -arena are required")
	}
	if *rung != "whole" && *rung != "partial" {
		return fmt.Errorf("-rung is whole or partial")
	}
	fx, err := loadSets(*arena, *baseline)
	if err != nil {
		return err
	}
	ms, err := cmp.ReadMasks(filepath.Join(*arena, "check", "masks.json"))
	if err != nil {
		return err
	}
	allEqual := true
	for pass := 1; pass <= *passes; pass++ {
		var before, last counters
		if *statsURL != "" {
			if before, err = readStats(*statsURL); err != nil {
				return err
			}
			last = before
		}
		total, equal, sent := 0, 0, 0
		var us, fetchedUS []int64
		saved := map[string][]cmp.Result{}
		shown := map[string]int{}
		for _, s := range fx {
			for _, c := range s.calls {
				if *only != "" && c.Tool != *only {
					continue
				}
				if pass == 1 && *killPID != 0 && sent == *killAt {
					if err := syscall.Kill(*killPID, syscall.SIGKILL); err != nil {
						return fmt.Errorf("kill %d: %v", *killPID, err)
					}
				}
				sent++
				total++
				start := time.Now()
				var rep keeperReply
				status, perr := post(*keeperURL+"/v1/call", callBody{Tool: c.Tool, Args: c.Args, Rung: *rung}, &rep)
				took := time.Since(start).Microseconds()
				us = append(us, took)
				if *statsURL != "" {
					now, err := readStats(*statsURL)
					if err != nil {
						return err
					}
					if now.Requests > last.Requests {
						fetchedUS = append(fetchedUS, took)
					}
					last = now
				}
				got := cmp.Result{ID: c.ID, Data: rep.Data, Text: rep.Text, Error: rep.Error, US: took}
				saved[s.name] = append(saved[s.name], got)
				var paths []string
				switch w, ok := s.want[c.ID]; {
				case perr != nil:
					paths = []string{"transport: " + perr.Error()}
				case status != http.StatusOK:
					paths = []string{"status " + strconv.Itoa(status)}
				case !ok:
					paths = []string{"baseline:missing"}
				default:
					paths = cmp.Diff(w, got, ms[s.name][c.ID])
				}
				if len(paths) == 0 {
					equal++
					continue
				}
				if shown[c.Tool] < 3 {
					shown[c.Tool]++
					if len(paths) > 8 {
						paths = append(paths[:8], fmt.Sprintf("... %d more", len(paths)-8))
					}
					fmt.Fprintf(os.Stderr, "diff pass=%d set=%s id=%d tool=%s paths=%s\n", pass, s.name, c.ID, c.Tool, strings.Join(paths, ","))
				}
			}
		}
		if pass == 1 && *save != "" {
			if err := saveResults(*save, saved); err != nil {
				return err
			}
		}
		line := fmt.Sprintf("pass=%d rung=%s total=%d equal=%d p50_ms=%.2f", pass, *rung, total, equal, p50(us))
		if *statsURL != "" {
			after, err := readStats(*statsURL)
			if err != nil {
				return err
			}
			line += fmt.Sprintf(" requests=%d bytes=%d fetched_calls=%d fetched_p50_ms=%.2f fetched_max_ms=%.2f", after.Requests-before.Requests, after.Bytes-before.Bytes, len(fetchedUS), p50(fetchedUS), maxMS(fetchedUS))
		}
		fmt.Println(line)
		if equal != total || total == 0 {
			allEqual = false
		}
	}
	if !allEqual {
		return fmt.Errorf("answers differ from the baseline")
	}
	return nil
}

func p50(us []int64) float64 {
	if len(us) == 0 {
		return 0
	}
	s := append([]int64(nil), us...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return float64(s[(len(s)-1)/2]) / 1000
}

func maxMS(us []int64) float64 {
	var m int64
	for _, u := range us {
		m = max(m, u)
	}
	return float64(m) / 1000
}

func saveResults(dir string, by map[string][]cmp.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, rs := range by {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		for _, r := range rs {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func ingest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	keeperURL := fs.String("keeper", "", "keeper base URL")
	fs.Parse(args)
	var out struct{ Gen, Seq, Parts uint64 }
	status, err := post(*keeperURL+"/v1/ingest", struct{}{}, &out)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("ingest answered status %d", status)
	}
	fmt.Printf("gen=%d seq=%d parts=%d\n", out.Gen, out.Seq, out.Parts)
	return nil
}

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	dir := fs.String("dir", "", "file store dir")
	tenant := fs.String("tenant", "", "tenant")
	wantGen := fs.Int64("gen", -1, "fail unless the latest manifest has this gen")
	fs.Parse(args)
	mdir := filepath.Join(*dir, "t", *tenant, "m")
	ents, err := os.ReadDir(mdir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("no manifest under %s", mdir)
	}
	sort.Strings(names)
	b, err := os.ReadFile(filepath.Join(mdir, names[len(names)-1]))
	if err != nil {
		return err
	}
	var m keeper.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	missing := 0
	for _, p := range m.Parts {
		if _, err := os.Stat(filepath.Join(*dir, string(p.Key))); err != nil {
			missing++
		}
	}
	claimed := int64(-1)
	if gs, err := os.ReadDir(filepath.Join(*dir, "t", *tenant, "g")); err == nil {
		for _, g := range gs {
			if n, err := strconv.ParseUint(g.Name(), 16, 64); err == nil && int64(n) > claimed {
				claimed = int64(n)
			}
		}
	}
	fmt.Printf("gen=%d seq=%d parts=%d missing=%d claimed=%d manifests=%d\n", m.Gen, m.Seq, len(m.Parts), missing, claimed, len(names))
	if missing > 0 {
		return fmt.Errorf("%d parts the manifest names are missing", missing)
	}
	if *wantGen >= 0 && int64(m.Gen) != *wantGen {
		return fmt.Errorf("latest manifest is gen %d, want %d", m.Gen, *wantGen)
	}
	return nil
}

func tools(args []string) error {
	fs := flag.NewFlagSet("tools", flag.ExitOnError)
	arena := fs.String("arena", os.Getenv("ARENA"), "fixture dir")
	fs.Parse(args)
	fx, err := loadSets(*arena, "")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, s := range fx {
		for _, c := range s.calls {
			if !seen[c.Tool] {
				seen[c.Tool] = true
				fmt.Println(c.Tool)
			}
		}
	}
	return nil
}

func stats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	url := fs.String("url", "", "file store /stats URL")
	fs.Parse(args)
	c, err := readStats(*url)
	if err != nil {
		return err
	}
	fmt.Printf("requests=%d bytes=%d\n", c.Requests, c.Bytes)
	return nil
}

func ports(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: e2e ports N")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return err
	}
	ls := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range ls {
			l.Close()
		}
	}()
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		ls = append(ls, l)
		fmt.Println(l.Addr().(*net.TCPAddr).Port)
	}
	return nil
}

func wait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	addr := fs.String("addr", "", "host:port")
	timeout := fs.Duration("timeout", 20*time.Second, "give up after")
	fs.Parse(args)
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", *addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s did not accept connections within %s", *addr, *timeout)
}

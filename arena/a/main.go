// Command a is a DuckDB-free mnemonic store prototype: per-source parsed
// logs plus one immutable, mmapped index, behind the MCP tools' Call shape.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

type Tool interface {
	Call(db *DB, args json.RawMessage) (any, string, error)
}

type ArgError struct{ Msg string }

func (e *ArgError) Error() string { return e.Msg }

var tools = map[string]Tool{
	"search_sessions":   searchSessionsTool{},
	"file_attention":    fileAttention{},
	"open":              openTool{},
	"list_sessions":     listSessions{},
	"get_ingest_status": getIngestStatus{},
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: a ingest|batch -root R -store S")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	root := fs.String("root", "", "data root")
	store := fs.String("store", "", "store dir")
	fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "ingest":
		err = ingest(*root, *store)
	case "batch":
		err = batch(*root, *store)
	default:
		err = fmt.Errorf("unknown command %s", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func ingest(root, store string) error {
	debug.SetGCPercent(25)
	if err := os.MkdirAll(srcDir(store), 0o755); err != nil {
		return err
	}
	sources, err := listSources(root)
	if err != nil {
		return err
	}
	work := make(chan source)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				if _, err := ingestSource(root, store, s); err != nil {
					fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", s.mpath, err)
				}
			}
		}()
	}
	for _, s := range sources {
		work <- s
	}
	close(work)
	wg.Wait()
	var cks []ckpt
	var srcs []stSource
	for _, s := range sources {
		row := stSource{Author: s.key.Author, Machine: s.key.Machine, UpdatedMs: s.src.UpdatedMs, Size: s.src.Size}
		if c, ok := readCkpt(store, sourceKey(s.key)); ok {
			cks = append(cks, c)
			row.Events = c.Rows
		}
		srcs = append(srcs, row)
	}
	parts := planParts(cks)
	ids := make([]string, 0, len(parts))
	if err := os.MkdirAll(filepath.Join(store, "parts"), 0o755); err != nil {
		return err
	}
	for _, members := range parts {
		id := fingerprint(members)
		if _, err := os.Stat(partDir(store, id)); err != nil {
			if err := buildIndex(store, members, id, partDir(store, id)); err != nil {
				return err
			}
		}
		ids = append(ids, id)
	}
	mb, _ := json.Marshal(manifestFile{Parts: ids, Sources: srcs})
	if cur, err := os.ReadFile(filepath.Join(store, "CURRENT")); err == nil && bytes.Equal(cur, mb) {
		return nil
	}
	if err := writeFileAtomic(filepath.Join(store, "CURRENT"), mb); err != nil {
		return err
	}
	gcParts(store, ids)
	return gcLogs(store, cks)
}

// planParts groups sources that share a session into one component and
// components into partShards parts by a hash of their least session id, so a
// session and every event version that can compete for its uids live in one
// part, and one source changing rebuilds only its part.
func planParts(cks []ckpt) [][]ckpt {
	parent := make([]int, len(cks))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	owner := map[string]int{}
	for i, c := range cks {
		for _, s := range c.Sessions {
			if j, ok := owner[s]; ok {
				parent[find(i)] = find(j)
			} else {
				owner[s] = i
			}
		}
	}
	least := map[int]string{}
	for i, c := range cks {
		r := find(i)
		k := c.Key
		if len(c.Sessions) > 0 {
			k = c.Sessions[0]
		}
		if cur, ok := least[r]; !ok || k < cur {
			least[r] = k
		}
	}
	shard := map[uint32][]ckpt{}
	for i, c := range cks {
		h := fnv.New32a()
		h.Write([]byte(least[find(i)]))
		s := h.Sum32() % partShards
		shard[s] = append(shard[s], c)
	}
	keys := make([]uint32, 0, len(shard))
	for k := range shard {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([][]ckpt, 0, len(keys))
	for _, k := range keys {
		out = append(out, shard[k])
	}
	return out
}

const partShards = 256

// gcParts removes part dirs the manifest does not name, and any half-built
// part a killed build left behind.
func gcParts(store string, keep []string) {
	want := map[string]bool{}
	for _, id := range keep {
		want[id] = true
	}
	ents, _ := os.ReadDir(filepath.Join(store, "parts"))
	for _, e := range ents {
		if !want[e.Name()] {
			os.RemoveAll(filepath.Join(store, "parts", e.Name()))
		}
	}
}

// gcLogs removes log generations no checkpoint names.
func gcLogs(store string, cks []ckpt) error {
	keep := map[string]bool{}
	for _, c := range cks {
		keep[filepath.Base(logPath(store, c.Key, c.Gen, "light"))] = true
		keep[filepath.Base(logPath(store, c.Key, c.Gen, "heavy"))] = true
		keep[filepath.Base(ckptPath(store, c.Key))] = true
	}
	ents, err := os.ReadDir(srcDir(store))
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !keep[e.Name()] {
			os.Remove(filepath.Join(srcDir(store), e.Name()))
		}
	}
	return nil
}

type call struct {
	ID   json.RawMessage `json:"id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type reply struct {
	ID    json.RawMessage `json:"id"`
	Data  any             `json:"data"`
	Text  string          `json:"text"`
	Error string          `json:"error"`
	Us    int64           `json:"us"`
}

func batch(root, store string) error {
	db, err := openDB(store)
	if err != nil {
		return err
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
		line, err := in.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var c call
			rep := reply{}
			if jerr := json.Unmarshal(line, &c); jerr != nil {
				rep.Error = jerr.Error()
			} else {
				rep.ID = c.ID
				start := time.Now()
				data, text, cerr := dispatch(db, c)
				rep.Us = time.Since(start).Microseconds()
				rep.Data, rep.Text = data, text
				if cerr != nil {
					rep.Error = cerr.Error()
				}
			}
			if err := enc.Encode(rep); err != nil {
				return err
			}
			if err := out.Flush(); err != nil {
				return err
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrClosed) || err.Error() == "EOF" {
				return nil
			}
			return nil
		}
	}
}

func dispatch(db *DB, c call) (data any, text string, err error) {
	defer func() {
		if p := recover(); p != nil {
			data, text, err = nil, "", fmt.Errorf("panic: %v", p)
		}
	}()
	t, ok := tools[c.Tool]
	if !ok {
		return nil, "", fmt.Errorf("unknown tool %s", c.Tool)
	}
	args := c.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return t.Call(db, args)
}

func lsObjectFields(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, err
	}
	for key := range fields {
		ok := false
		for _, a := range allowed {
			if key == a {
				ok = true
				break
			}
		}
		if !ok {
			return nil, argErrorf("unknown field %q", key)
		}
	}
	return fields, nil
}

func stringAt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func intAt(m map[string]any, key string) int {
	n, _ := intAtOK(m, key)
	return n
}

func intAtOK(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	n, ok := m[key].(int)
	return n, ok
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

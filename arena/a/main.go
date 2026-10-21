// Command a is a DuckDB-free mnemonic store prototype: per-source parsed
// logs plus one immutable, mmapped index, behind the MCP tools' Call shape.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Tool interface {
	Call(st *Store, args json.RawMessage) (any, string, error)
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
	if err := os.MkdirAll(srcDir(store), 0o755); err != nil {
		return err
	}
	sources, err := listSources(root)
	if err != nil {
		return err
	}
	var cks []ckpt
	var srcs []stSource
	for _, s := range sources {
		if _, err := ingestSource(root, store, s); err != nil {
			fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", s.mpath, err)
		}
		row := stSource{Author: s.key.Author, Machine: s.key.Machine, UpdatedMs: s.src.UpdatedMs, Size: s.src.Size}
		if c, ok := readCkpt(store, sourceKey(s.key)); ok {
			cks = append(cks, c)
			row.Events = c.Rows
		}
		srcs = append(srcs, row)
	}
	sb, _ := json.Marshal(srcs)
	fp := fingerprint(cks) + fmt.Sprintf("%x", sha256.Sum256(sb))
	if cur, err := currentMeta(store); err == nil && cur.Fingerprint == fp {
		return nil
	}
	if err := buildIndex(store, cks, fp, sb); err != nil {
		return err
	}
	return gcLogs(store, cks)
}

func currentMeta(store string) (indexMeta, error) {
	var m indexMeta
	cur, err := os.ReadFile(filepath.Join(store, "CURRENT"))
	if err != nil {
		return m, err
	}
	b, err := os.ReadFile(filepath.Join(store, strings.TrimSpace(string(cur)), "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
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
	st, err := openStore(store)
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
				data, text, cerr := dispatch(st, c)
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

func dispatch(st *Store, c call) (data any, text string, err error) {
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
	return t.Call(st, args)
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

// Package duck is today's DuckDB path (lake, then cache.Converge, then
// query.Call) behind the arena contract's ingest and batch commands.
package duck

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/lake"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/query"
)

var opts = cache.Options{MemoryLimit: "800MB", FlagsSQL: query.SearchFlagsSQL()}

type Stats struct {
	Lake     lake.Stats
	Converge cache.ConvergeStats
}

// Ingest parses every raw source under dataDir/raw into the lake and converges
// the index, running the parser sweep to its end.
func Ingest(dataDir string) (Stats, error) {
	var st Stats
	sweep := lake.Sweep{}
	for {
		ls, err := lake.Run(dataDir, func() bool { return false }, sweep)
		if err != nil {
			return st, err
		}
		st.Lake.Sources, st.Lake.Rewrote, st.Lake.Failed = ls.Sources, st.Lake.Rewrote+ls.Rewrote, st.Lake.Failed+ls.Failed
		st.Lake.Events += ls.Events
		if ls.Next == (lake.Sweep{}) {
			break
		}
		sweep = ls.Next
	}
	ix, err := cache.Open(dataDir, opts)
	if err != nil {
		return st, err
	}
	defer ix.Close()
	st.Converge, err = ix.Converge(context.Background(), cache.RetryFailed)
	return st, err
}

type Call struct {
	ID   int             `json:"id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type Result struct {
	ID    int    `json:"id"`
	Data  any    `json:"data"`
	Text  string `json:"text"`
	Error string `json:"error"`
	US    int64  `json:"us"`
}

// Batch answers one JSONL call per line of in with one result line on out;
// edit, when set, rewrites each result before it is written.
func Batch(dataDir string, in io.Reader, out io.Writer, edit func(tool string, r *Result)) error {
	ix, err := cache.Open(dataDir, opts)
	if err != nil {
		return err
	}
	defer ix.Close()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	w := bufio.NewWriter(out)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return fmt.Errorf("call: %w", err)
		}
		r := Result{ID: c.ID}
		tool, ok := query.Lookup(c.Tool)
		if !ok {
			r.Error = "unknown tool: " + c.Tool
		} else {
			args := c.Args
			if len(args) == 0 {
				args = json.RawMessage("null")
			}
			start := time.Now()
			data, text, err := tool.Call(context.Background(), ix.DB(), args)
			r.US = time.Since(start).Microseconds()
			r.Data, r.Text = data, text
			if err != nil {
				r.Error = err.Error()
			}
		}
		if edit != nil {
			edit(c.Tool, &r)
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		w.Write(append(b, '\n'))
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return sc.Err()
}

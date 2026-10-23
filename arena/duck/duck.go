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

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/lake"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/query"
	"github.com/candyflipline/mnemonic/internal/store"
)

var opts = cache.Options{MemoryLimit: "800MB", FlagsSQL: query.SearchFlagsSQL()}

// token is the bearer token the batch's identity is built from; only its
// hash is looked up in a one-entry table.
const token = "arena-oracle"

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
// edit, when set, rewrites each result before it is written. Author is who
// every tool acts as: each call's ctx carries what the server puts under an
// MCP request (server.New) — an identity for author with every scope in the
// default tenant, the store under dataDir, and the session controls read
// from dataDir — so the write tools append through the store and the scoped
// reads filter exactly as `mnemonic serve` would.
func Batch(dataDir, author string, in io.Reader, out io.Writer, edit func(tool string, r *Result)) error {
	ix, err := cache.Open(dataDir, opts)
	if err != nil {
		return err
	}
	defer ix.Close()
	ctl, err := query.LoadControls(dataDir)
	if err != nil {
		return err
	}
	ctx := batchCtx(dataDir, author, ctl)
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
			data, text, err := tool.Call(ctx, ix.DB(), args)
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

// batchCtx is the ctx the server would serve one MCP request under: the
// identity of a token held by author (every scope, default tenant), the
// store whose appends write under dataDir/raw, and ctl.
func batchCtx(dataDir, author string, ctl *query.Controls) context.Context {
	tokens := auth.NewTokens(map[string]auth.Token{
		auth.Hash(token): {Author: author, Scopes: []auth.Scope{auth.Read, auth.Push, auth.Annotate, auth.Admin}},
	})
	ctx, _ := tokens.Context(context.Background(), token)
	wake := make(chan struct{}, 1)
	ctx = store.WithStore(ctx, &store.Store{DataDir: dataDir, Wake: wake})
	return query.WithControls(ctx, ctl)
}

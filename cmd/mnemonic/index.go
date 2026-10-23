package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/candyflipline/mnemonic/internal/index"
)

func init() { commands["index"] = indexCmd }

// indexCmd is the engine's harness entry, kept out of usage:
//
//	mnemonic index ingest -root R -store S [-quiet D] [-settle]
//	mnemonic index batch -root R -store S    one JSON call per stdin line, one reply per stdout line
//
// It speaks arena/a's line protocol, so arena/check scores it unchanged.
func indexCmd(args []string) error {
	if len(args) < 1 || (args[0] != "ingest" && args[0] != "batch") {
		return fmt.Errorf("usage: mnemonic index ingest|batch -root R -store S")
	}
	fs := flag.NewFlagSet("index "+args[0], flag.ContinueOnError)
	root := fs.String("root", "", "data root holding raw/")
	store := fs.String("store", "", "index store dir")
	quiet := fs.Duration("quiet", 15*time.Minute, "a source settles into its cold part once its updated_ms is this far behind the newest source's")
	settle := fs.Bool("settle", false, "settle every source now")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "ingest" {
		st, err := index.Ingest(*root, *store, *quiet, *settle)
		fmt.Fprintf(os.Stderr, "ingest: unchanged=%d resumed=%d fresh=%d reparsed=%d\n",
			st[index.Unchanged], st[index.Resumed], st[index.Fresh], st[index.Reparsed])
		return err
	}
	return indexBatch(*store)
}

type batchCall struct {
	ID   json.RawMessage `json:"id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type batchReply struct {
	ID    json.RawMessage `json:"id"`
	Data  json.RawMessage `json:"data"`
	Text  string          `json:"text"`
	Error string          `json:"error"`
	Us    int64           `json:"us"`
}

func indexBatch(store string) error {
	ix, err := index.Open(store)
	if err != nil {
		return err
	}
	defer ix.Close()
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriterSize(os.Stdout, 1<<20)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for {
		line, rerr := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var c batchCall
			rep := batchReply{}
			if err := json.Unmarshal(line, &c); err != nil {
				rep.Error = err.Error()
			} else {
				rep.ID = c.ID
				start := time.Now()
				db, release := ix.Snapshot()
				data, text, cerr := index.Call(context.Background(), db, c.Tool, c.Args)
				release()
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
		if rerr != nil {
			return nil
		}
	}
}

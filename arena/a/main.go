// Command a is a DuckDB-free mnemonic store prototype: per-source parsed
// logs plus one immutable, mmapped index, behind the MCP tools' Call shape.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/arena/trace"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: a ingest|batch|keep|read|probe ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "ingest", "batch":
		err = engine(os.Args[1], os.Args[2:])
	case "keep":
		err = runKeep(os.Args[2:])
	case "read":
		err = runRead(os.Args[2:])
	case "probe":
		err = runProbe(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %s", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func engine(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	root := fs.String("root", "", "data root")
	store := fs.String("store", "", "store dir")
	quiet := fs.Duration("quiet", 15*time.Minute, "a source settles into its cold part once its updated_ms is this far behind the newest source's")
	settle := fs.Bool("settle", false, "settle every source now")
	fs.Parse(args)
	if cmd == "ingest" {
		return trace.Ingest(*root, *store, *quiet, *settle)
	}
	return batch(*root, *store)
}

type call struct {
	ID   json.RawMessage `json:"id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type reply struct {
	ID    json.RawMessage `json:"id"`
	Data  json.RawMessage `json:"data"`
	Text  string          `json:"text"`
	Error string          `json:"error"`
	Us    int64           `json:"us"`
}

func batch(root, store string) error {
	stamp, _ := trace.CurrentStamp(store)
	db, err := trace.OpenDB(store)
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
				if now, ok := trace.CurrentStamp(store); ok && now != stamp {
					if next, err := trace.OpenDB(store); err == nil {
						db.Close()
						db, stamp = next, now
					}
				}
				data, text, cerr := trace.Call(db, c.Tool, c.Args)
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

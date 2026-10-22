// Command duckref is the arena's positive control: today's DuckDB path behind
// the contract CLI. The store holds data/, whose raw is a link to the root's.
// Built with -ldflags "-X main.dropLast=1" it is the negative control: every
// search answer loses its last result.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/candyflipline/mnemonic/arena/duck"
)

var dropLast string

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: duckref ingest|batch -root R -store S"))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	root := fs.String("root", "", "data root holding raw/")
	store := fs.String("store", "", "store dir")
	fs.Parse(os.Args[2:])
	data, err := dataDir(*root, *store)
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "ingest":
		_, err = duck.Ingest(data)
	case "batch":
		var edit func(string, *duck.Result)
		if dropLast != "" {
			edit = dropLastResult
		}
		err = duck.Batch(data, os.Stdin, os.Stdout, edit)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func dataDir(root, store string) (string, error) {
	raw, err := filepath.Abs(filepath.Join(root, "raw"))
	if err != nil {
		return "", err
	}
	data := filepath.Join(store, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		return "", err
	}
	link := filepath.Join(data, "raw")
	if got, err := os.Readlink(link); err != nil || got != raw {
		os.Remove(link)
		if err := os.Symlink(raw, link); err != nil {
			return "", err
		}
	}
	return data, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "duckref:", err)
	os.Exit(1)
}

func dropLastResult(tool string, r *duck.Result) {
	if tool != "search_sessions" || r.Data == nil {
		return
	}
	b, err := json.Marshal(r.Data)
	if err != nil {
		return
	}
	var d map[string]any
	if json.Unmarshal(b, &d) != nil {
		return
	}
	if rs, ok := d["results"].([]any); ok && len(rs) > 0 {
		d["results"] = rs[:len(rs)-1]
	}
	r.Data = d
}

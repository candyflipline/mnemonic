// Command oracle builds today's DuckDB index over a copy of the arena raw,
// answers calls through query.Call, and speaks the arena contract's candidate
// protocol so check can score the oracle itself.
//
//	oracle build -raw RAWROOT -root ORACLEROOT
//	oracle answer -root ORACLEROOT < calls.jsonl > expected.jsonl
//	oracle ingest -root RAWROOT -store STOREROOT
//	oracle batch -root RAWROOT -store STOREROOT < calls.jsonl > results.jsonl
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/arena/duck"
)

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: oracle build|answer|ingest|batch"))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	raw := fs.String("raw", "", "build: arena data root whose raw/ is copied")
	root := fs.String("root", "", "build/answer: oracle data root; ingest/batch: candidate raw root")
	store := fs.String("store", "", "ingest/batch: candidate store (data dir)")
	author := fs.String("author", "", "who the tools act as (default: the first author=* dir under the data root's raw, else arena)")
	fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "build":
		err = build(*raw, *root)
	case "answer":
		err = answer(*root, *author)
	case "ingest":
		err = ingest(*root, *store)
	case "batch":
		err = batch(*store, *author)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func build(raw, root string) error {
	if _, err := os.Stat(filepath.Join(root, "raw")); err == nil {
		return fmt.Errorf("%s already holds raw/; build into a fresh root", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := copyRaw(raw, root); err != nil {
		return err
	}
	start := time.Now()
	st, err := duck.Ingest(root)
	fmt.Fprintf(os.Stderr, "oracle build: sources=%d rewrote=%d failed=%d events=%d converge files=%d inserted=%d sessions=%d in %s\n",
		st.Lake.Sources, st.Lake.Rewrote, st.Lake.Failed, st.Lake.Events,
		st.Converge.Files, st.Converge.Inserted, st.Converge.Sessions, time.Since(start).Round(time.Millisecond))
	return err
}

func answer(root, author string) error {
	return duck.Batch(root, who(root, author), os.Stdin, os.Stdout, nil)
}

// ingest is the arena contract's candidate ingest: bring a store under
// dataDir up to date with everything under rawRoot/raw. The DuckDB path
// reads raw through its data root, so the store holds a copy of raw, refreshed
// from rawRoot on every ingest to pick up appends, and the lake and cache
// build next to it.
func ingest(rawRoot, dataDir string) error {
	if _, err := os.Stat(filepath.Join(rawRoot, "raw")); err != nil {
		return fmt.Errorf("%s holds no raw/", rawRoot)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	if err := copyRaw(rawRoot, dataDir); err != nil {
		return err
	}
	st, err := duck.Ingest(dataDir)
	fmt.Fprintf(os.Stderr, "oracle ingest: sources=%d rewrote=%d failed=%d events=%d converge files=%d inserted=%d sessions=%d\n",
		st.Lake.Sources, st.Lake.Rewrote, st.Lake.Failed, st.Lake.Events,
		st.Converge.Files, st.Converge.Inserted, st.Converge.Sessions)
	return err
}

// batch is the arena contract's candidate batch: answer calls from the store.
func batch(dataDir, author string) error {
	if _, err := os.Stat(filepath.Join(dataDir, "raw")); err != nil {
		return fmt.Errorf("%s holds no raw/; run ingest first", dataDir)
	}
	return duck.Batch(dataDir, who(dataDir, author), os.Stdin, os.Stdout, nil)
}

// copyRaw refreshes dataDir/raw from rawRoot/raw: the first call copies it,
// later calls overwrite each file so appends since the last ingest are seen.
func copyRaw(rawRoot, dataDir string) error {
	out, err := exec.Command("cp", "-Rp", filepath.Join(rawRoot, "raw")+"/", filepath.Join(dataDir, "raw")+"/").CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy raw: %v: %s", err, out)
	}
	return nil
}

// who is the flag when set, else the first author of the data root's raw,
// else arena.
func who(root, flag string) string {
	if flag != "" {
		return flag
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "raw", "author=*"))
	sort.Strings(dirs)
	if len(dirs) > 0 {
		return strings.TrimPrefix(filepath.Base(dirs[0]), "author=")
	}
	return "arena"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "oracle:", err)
	os.Exit(1)
}

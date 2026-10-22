// Command oracle builds today's DuckDB index over a copy of the arena raw and
// answers calls through query.Call.
//
//	oracle build -raw RAWROOT -root ORACLEROOT
//	oracle answer -root ORACLEROOT < calls.jsonl > expected.jsonl
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/candyflipline/mnemonic/arena/duck"
)

func main() {
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: oracle build|answer"))
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	raw := fs.String("raw", "", "arena data root whose raw/ is copied")
	root := fs.String("root", "", "oracle data root")
	fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "build":
		err = build(*raw, *root)
	case "answer":
		err = duck.Batch(*root, os.Stdin, os.Stdout, nil)
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
	if out, err := exec.Command("cp", "-Rp", filepath.Join(raw, "raw"), filepath.Join(root, "raw")).CombinedOutput(); err != nil {
		return fmt.Errorf("copy raw: %v: %s", err, out)
	}
	start := time.Now()
	st, err := duck.Ingest(root)
	fmt.Fprintf(os.Stderr, "oracle build: sources=%d rewrote=%d failed=%d events=%d converge files=%d inserted=%d sessions=%d in %s\n",
		st.Lake.Sources, st.Lake.Rewrote, st.Lake.Failed, st.Lake.Events,
		st.Converge.Files, st.Converge.Inserted, st.Converge.Sessions, time.Since(start).Round(time.Millisecond))
	return err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "oracle:", err)
	os.Exit(1)
}

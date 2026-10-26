//go:build server

package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/candyflipline/mnemonic/internal/export"
	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/layout"
)

func init() { commands["export"] = exportCmd }

const exportUsage = `usage: mnemonic export atif|otlp --data-dir D --session S [--out F]
       mnemonic export dataset --data-dir D --annotation N[=V] --per-task K [--include-private] [--out F]

export runs on the server's data dir and sees every author. It honours the
session controls there: a deleted session is refused ("session not found") by
atif and otlp and skipped by dataset, and dataset also skips private sessions
unless --include-private is given. A session that fails ATIF validation is
skipped by dataset with its id and error on stderr; dataset fails only when
every matching session did.`

// exportCmd writes sessions out of the index: one ATIF trajectory or OTLP
// trace per session, or a JSONL dataset filtered by annotation.
func exportCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(exportUsage)
	}
	kind, rest := args[0], args[1:]
	switch kind {
	case "atif", "otlp", "dataset":
	default:
		return fmt.Errorf("export: unknown kind %q; %s", kind, exportUsage)
	}
	fs := flag.NewFlagSet("export "+kind, flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory holding raw/ and index/ (required)")
	session := fs.String("session", "", "session id (atif, otlp)")
	annotation := fs.String("annotation", "", "name[=value] to filter sessions by (dataset)")
	perTask := fs.Int("per-task", 3, "at most this many sessions per task (dataset)")
	includePrivate := fs.Bool("include-private", false, "also export private sessions (dataset)")
	out := fs.String("out", "", "output file (default stdout)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *dataDir == "" {
		return errors.New("export: --data-dir is required")
	}
	var (
		sessionID string
		err       error
	)
	if *session != "" {
		if sessionID, err = rawSessionID(*session); err != nil {
			return err
		}
	}
	if kind == "dataset" {
		if *annotation == "" {
			return errors.New("export: --annotation is required")
		}
	} else if sessionID == "" {
		return errors.New("export: --session is required")
	}

	controls, err := index.LoadControls(*dataDir)
	if err != nil {
		return err
	}
	// A named session is the operator's choice, so only a dataset sweep skips
	// private ones.
	hidden := controls.ExportHidden(kind != "dataset" || *includePrivate)

	// One-shot: bring the raw under the data dir into the index, everything
	// settled, as the converge before an export did. The index dir's writer
	// lock says whether that is ours to do: under a running writer (the
	// server) it is not, and export exports the published CURRENT without
	// ingesting, saying so on stderr.
	ix, err := index.Open(layout.IndexDir(*dataDir))
	if err != nil {
		return err
	}
	defer ix.Close()
	lock, lockErr := index.HoldIndexLock(layout.IndexDir(*dataDir))
	switch {
	case errors.Is(lockErr, index.ErrLocked):
		fmt.Fprintf(os.Stderr, "export: %v; exporting the published index without ingesting\n", lockErr)
	case lockErr != nil:
		return lockErr
	default:
		defer lock.Unlock()
		if _, err := ix.Ingest(*dataDir, index.IngestOptions{SettleAll: true}); err != nil {
			return err
		}
	}
	db, release := ix.Snapshot()
	defer release()

	w := &outFile{path: *out}
	switch kind {
	case "atif":
		err = export.WriteATIF(w, db, sessionID, hidden)
	case "otlp":
		err = export.WriteOTLP(w, db, sessionID, hidden)
	case "dataset":
		f, parseErr := parseAnnotation(*annotation)
		if parseErr != nil {
			return parseErr
		}
		var skipped []export.SessionError
		skipped, err = export.Dataset(w, db, f, *perTask, hidden)
		for _, s := range skipped {
			fmt.Fprintf(os.Stderr, "export: skipped session %s: %v\n", s.SessionID, s.Err)
		}
	}
	return w.finish(err)
}

// outFile is --out, or stdout for "" and "-", written through a buffer. The
// file is created at the first write, or empty when an export that wrote
// nothing succeeds; an export that fails after writing removes it. Stdout gets
// a final newline when the export did not end in one.
type outFile struct {
	path string
	f    *os.File
	buf  *bufio.Writer
	last byte
}

func (o *outFile) toStdout() bool { return o.path == "" || o.path == "-" }

func (o *outFile) open() error {
	if o.buf != nil {
		return nil
	}
	dst := os.Stdout
	if !o.toStdout() {
		f, err := os.OpenFile(o.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		o.f, dst = f, f
	}
	o.buf = bufio.NewWriterSize(dst, 1<<16)
	return nil
}

func (o *outFile) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := o.open(); err != nil {
		return 0, err
	}
	o.last = p[len(p)-1]
	return o.buf.Write(p)
}

func (o *outFile) finish(err error) error {
	if err == nil {
		if err = o.open(); err == nil && o.toStdout() && o.last != 0 && o.last != '\n' {
			err = o.buf.WriteByte('\n')
		}
		if err == nil {
			err = o.buf.Flush()
		}
	}
	if o.f != nil {
		if cerr := o.f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(o.path)
		}
	}
	return err
}

// parseAnnotation splits --annotation's name[=value].
func parseAnnotation(s string) (export.AnnotationFilter, error) {
	name, value, hasValue := strings.Cut(s, "=")
	if name == "" {
		return export.AnnotationFilter{}, errors.New("export: --annotation needs a name")
	}
	return export.AnnotationFilter{Name: name, Value: value, HasValue: hasValue}, nil
}

// rawSessionID accepts the raw session id and the MCP-facing
// session:<base64> form (59796bc:internal/query/common.go:166).
func rawSessionID(s string) (string, error) {
	raw, ok := strings.CutPrefix(s, "session:")
	if !ok {
		return s, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("export: bad session id %q: %w", s, err)
	}
	return string(decoded), nil
}

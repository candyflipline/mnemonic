package main

import (
	"os"
	"path/filepath"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// A source transaction may publish several bounded intervals together. Parser
// state is advanced only after every interval has been finished durably.
const segmentTextBudget = 8 << 20

type sourceWriter struct {
	dir                      string
	codecs                   *segmentCodecs
	current                  *segmentWriter
	names                    []string
	count                    uint64
	maxVersion, versionFloor uint64
	budget                   int
}

func newSourceWriter(dir string, c *segmentCodecs, previous uint64, budget int) *sourceWriter {
	return &sourceWriter{dir: dir, codecs: c, maxVersion: previous, versionFloor: previous, budget: budget}
}
func (w *sourceWriter) emit(e *schema.Event) error {
	if w.current != nil && w.current.inputBytes >= w.budget && w.current.count > 0 {
		name, err := w.current.finish()
		if err != nil {
			return err
		}
		w.names = append(w.names, name)
		w.current = nil
	}
	if w.current == nil {
		var err error
		w.current, err = newSegmentWriter(w.dir, w.codecs)
		if err != nil {
			return err
		}
	}
	if w.versionFloor != 0 {
		e.EventVersion = max(e.EventVersion, w.versionFloor+1)
	}
	w.maxVersion = max(w.maxVersion, e.EventVersion)
	if err := w.current.emit(e); err != nil {
		return err
	}
	w.count++
	return nil
}
func (w *sourceWriter) finish() ([]string, error) {
	if w.current == nil && len(w.names) == 0 {
		var err error
		w.current, err = newSegmentWriter(w.dir, w.codecs)
		if err != nil {
			return nil, err
		}
	}
	if w.current != nil {
		name, err := w.current.finish()
		if err != nil {
			w.abort()
			return nil, err
		}
		w.names = append(w.names, name)
		w.current = nil
	}
	return w.names, nil
}
func (w *sourceWriter) abort() {
	if w.current != nil {
		w.current.abort()
		w.current = nil
	}
	for _, name := range w.names {
		os.RemoveAll(filepath.Join(w.dir, name))
	}
	w.names = nil
}

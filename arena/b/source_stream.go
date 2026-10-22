package main

import (
	"fmt"
	"sync"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// The parser remains serial, preserving source/event version order. Up to two
// bounded writers overlap compression, token indexing and durable file writes
// with parsing the next source. Each codec pair belongs to one worker at a time.
type sourceStream struct {
	events               chan schema.Event
	cancel, done         chan struct{}
	sealOnce, cancelOnce sync.Once
	names                []string
	count, maxVersion    uint64
	err                  error
}

func newSourceStream(dir string, pool chan *segmentCodecs, previous uint64, prior []string) *sourceStream {
	prior = append([]string(nil), prior...)
	s := &sourceStream{events: make(chan schema.Event, 8), cancel: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		var codecs *segmentCodecs
		select {
		case codecs = <-pool:
		case <-s.cancel:
			s.err = fmt.Errorf("source write canceled")
			return
		}
		defer func() { pool <- codecs }()
		w := newSourceWriter(dir, codecs, previous, segmentTextBudget)
		for {
			select {
			case <-s.cancel:
				w.abort()
				s.err = fmt.Errorf("source write canceled")
				return
			default:
			}
			select {
			case <-s.cancel:
				w.abort()
				s.err = fmt.Errorf("source write canceled")
				return
			case ev, ok := <-s.events:
				if !ok {
					s.names, s.err = w.finish()
					if s.err == nil {
						s.names, s.err = compactSourceSegments(dir, append(prior, s.names...), codecs)
					}
					s.count = w.count
					s.maxVersion = w.maxVersion
					return
				}
				if s.err = w.emit(&ev); s.err != nil {
					w.abort()
					return
				}
			}
		}
	}()
	return s
}
func (s *sourceStream) emit(e *schema.Event) error {
	select {
	case s.events <- *e:
		return nil
	case <-s.done:
		return s.err
	}
}
func (s *sourceStream) seal()       { s.sealOnce.Do(func() { close(s.events) }) }
func (s *sourceStream) abort()      { s.cancelOnce.Do(func() { close(s.cancel) }); <-s.done }
func (s *sourceStream) wait() error { <-s.done; return s.err }

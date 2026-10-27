//go:build server

package main

import (
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tlslogCap bounds the distinct remote addresses remembered per minute; past
// it the summary says 1024+ instead of a false exact count.
const tlslogCap = 1024

// tlslogPrefix is the exact prefix net/http writes for a failed handshake
// (net/http server.go, conn.serve): "http: TLS handshake error from
// <remote>: <reason>".
const tlslogPrefix = "http: TLS handshake error from "

// tlsLogWriter is http.Server's ErrorLog destination: it counts TLS handshake
// errors and reports one summary line per minute (count and distinct remote
// address count) instead of one line per connection, and passes every other
// line through to the standard logger unchanged.
type tlsLogWriter struct {
	listen string

	mu      sync.Mutex
	count   int
	ips     map[string]struct{}
	overCap bool

	stop    chan struct{}
	stopped chan struct{}
}

func newTLSLogWriter(listen string) *tlsLogWriter {
	w := &tlsLogWriter{
		listen:  listen,
		ips:     make(map[string]struct{}),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go w.run()
	return w
}

// Stop ends the minute ticker and waits for it, so nothing outlives serve.
func (w *tlsLogWriter) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.stopped
}

func (w *tlsLogWriter) run() {
	defer close(w.stopped)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.summary()
		case <-w.stop:
			return
		}
	}
}

// summary logs the last minute's line, only when something was counted, then
// resets the window.
func (w *tlsLogWriter) summary() {
	w.mu.Lock()
	count, n := w.count, len(w.ips)
	over := w.overCap
	w.count = 0
	w.ips = make(map[string]struct{})
	w.overCap = false
	w.mu.Unlock()
	if count == 0 {
		return
	}
	from := comma(n)
	if over {
		from += "+"
	}
	unit := "address"
	if n != 1 {
		unit = "addresses"
	}
	log.Printf("http %s: %s TLS handshake errors in the last minute from %s %s", w.listen, comma(count), from, unit)
}

func (w *tlsLogWriter) Write(p []byte) (int, error) {
	line := strings.TrimSuffix(string(p), "\n")
	if rest, ok := strings.CutPrefix(line, tlslogPrefix); ok {
		w.countHandshake(remoteHost(rest))
	} else {
		log.Print(line)
	}
	return len(p), nil
}

// countHandshake records one handshake error from host.
func (w *tlsLogWriter) countHandshake(host string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.count++
	if w.overCap {
		return
	}
	if _, ok := w.ips[host]; ok {
		return
	}
	if len(w.ips) >= tlslogCap {
		w.overCap = true
		return
	}
	w.ips[host] = struct{}{}
}

// remoteHost takes the "<remote>: <reason>" tail of a handshake line and
// returns the remote's host part; anything it cannot split is returned whole.
func remoteHost(rest string) string {
	if i := strings.Index(rest, ": "); i >= 0 {
		rest = rest[:i]
	}
	if host, _, err := net.SplitHostPort(rest); err == nil {
		return host
	}
	return rest
}

// comma groups a count in thousands, as in 9,600.
func comma(n int) string {
	s := strconv.Itoa(n)
	pre := len(s) % 3
	var b strings.Builder
	b.WriteString(s[:pre])
	for ; pre < len(s); pre += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[pre : pre+3])
	}
	return b.String()
}

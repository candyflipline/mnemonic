// Package push is the client side of the wire protocol: scan the configured
// globs, diff against the server's manifest, send only whole redacted lines.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
)

type Redactor interface {
	Line(line []byte) []byte
}

type Stats struct {
	Scanned, Appended, Replaced, Unchanged, Failed int
	Bytes                                          int64
}

// manifestPath is the only wire path without a constant in internal/wire.
const manifestPath = "/v1/manifest"

// skewMs is the mtime-vs-UpdatedMs margin the skip needs, because the client
// and server clocks need not agree.
const skewMs = 5 * 60 * 1000

type fileRef struct {
	path       string
	harness    string
	sourceName string
	mtimeMs    int64
	size       int64
}

func Run(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client) (Stats, error) {
	manifest, err := fetchManifest(ctx, cfg, hc)
	if err != nil {
		return Stats{}, err
	}
	var st Stats
	for _, f := range scan(cfg.Sources) {
		st.Scanned++
		if err := syncSource(ctx, cfg, red, hc, manifest, f, &st); err != nil {
			st.Failed++
		}
	}
	return st, nil
}

func fetchManifest(ctx context.Context, cfg config.Client, hc *http.Client) (map[string]*wire.Source, error) {
	u := strings.TrimSuffix(cfg.Server, "/") + manifestPath + "?" + url.Values{"machine": {cfg.Machine}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest: %s: %s", resp.Status, readErrBody(resp.Body))
	}
	var man wire.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&man); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	byID := make(map[string]*wire.Source, len(man.Sources))
	for i := range man.Sources {
		byID[man.Sources[i].SrcID] = &man.Sources[i]
	}
	return byID, nil
}

func syncSource(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client, manifest map[string]*wire.Source, f fileRef, st *Stats) error {
	srcID := wire.SrcID(f.path)
	entry := manifest[srcID]
	if entry != nil && entry.RawEnd > 0 && f.size == entry.RawEnd {
		st.Unchanged++
		return nil
	}
	// The server stamps UpdatedMs after it received the bytes, so a file whose
	// last write is older than that stamp (with a 5-minute margin for
	// client/server clock skew) has nothing new; skip it unread. Entries with
	// RawEnd 0 — the legacy shape — are saved the read by this skip alone.
	if entry != nil && f.mtimeMs < entry.UpdatedMs-skewMs {
		st.Unchanged++
		return nil
	}
	orig := entry // raw_end as the manifest first had it; retries below rewrite entry
	sent, replaced := false, false
	if entry != nil && entry.RawEnd > 0 && entry.Size > 0 {
		done, tailSent, next, err := syncTail(ctx, cfg, red, hc, f, srcID, entry, st)
		if err != nil {
			return err
		}
		if done {
			if tailSent {
				st.Appended++
			} else {
				st.Unchanged++
			}
			return nil
		}
		entry, sent = next, tailSent
	}
	redacted, head, rawEnds, ok, err := redactComplete(f.path, red)
	if err != nil {
		return err
	}
	if !ok {
		if !sent {
			st.Unchanged++
		}
		return nil
	}
	for conflicts := 0; ; {
		body, offset, noop := decide(entry, head, redacted)
		if noop {
			// A legacy entry (RawEnd 0) whose redacted bytes the server already
			// holds in full would read whole every pass forever: one empty
			// append at Size records raw_end so the next pass takes the exact
			// skip. Best effort — an old server answers 400 and a stale one
			// 409, and either way the file just stays on this full path.
			if orig != nil && orig.RawEnd == 0 && !sent {
				_, _ = putSource(ctx, cfg, hc, f, srcID, entry.Size, head, nil, rawEnds[len(rawEnds)-1])
			}
			switch {
			case !sent:
				st.Unchanged++
			case replaced:
				st.Replaced++
			default:
				st.Appended++
			}
			return nil
		}
		body = firstChunk(body, maxChunk)
		conflict, err := putSource(ctx, cfg, hc, f, srcID, offset, head, body, rawEndOf(rawEnds, redacted, offset, body))
		if err != nil {
			return err
		}
		if conflict != nil {
			if conflicts++; conflicts > 1 {
				return fmt.Errorf("put %s: conflict again at size %d", f.path, conflict.Size)
			}
			entry = &wire.Source{SrcID: srcID, Size: conflict.Size, HeadHash: conflict.HeadHash}
			continue
		}
		st.Bytes += int64(len(body))
		sent, replaced = true, replaced || offset == 0
		entry = &wire.Source{SrcID: srcID, Size: offset + int64(len(body)), HeadHash: head}
	}
}

// syncTail is the fast path: with the server vouching that its stored bytes
// end at raw offset entry.RawEnd of this file, only the bytes past it are
// read and redacted. It appends the new whole lines in chunks, each PUT
// carrying the raw_end its own body ends at, and reports done once the tail
// is sent or turns out empty. A 409 means our entry was stale, and the
// redacted-length coincidence the full path checks is the only proof left, so
// it hands the source back with the Conflict entry for the full path.
func syncTail(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client, f fileRef, srcID string, entry *wire.Source, st *Stats) (done, sent bool, next *wire.Source, err error) {
	tail, head, rawEnds, ok, err := redactTail(f.path, red, entry)
	if err != nil || !ok {
		return false, false, entry, err
	}
	for len(tail) > 0 {
		chunk := firstChunk(tail, maxChunk)
		lines := bytes.Count(chunk, []byte{'\n'})
		rawEnd := rawEnds[lines-1]
		conflict, err := putSource(ctx, cfg, hc, f, srcID, entry.Size, head, chunk, rawEnd)
		if err != nil {
			return true, sent, nil, err
		}
		if conflict != nil {
			return false, sent, &wire.Source{SrcID: srcID, Size: conflict.Size, HeadHash: conflict.HeadHash, RawEnd: conflict.RawEnd}, nil
		}
		st.Bytes += int64(len(chunk))
		sent = true
		tail, rawEnds = tail[len(chunk):], rawEnds[lines:]
		entry = &wire.Source{SrcID: srcID, Size: entry.Size + int64(len(chunk)), HeadHash: head, RawEnd: rawEnd}
	}
	return true, sent, nil, nil
}

// maxChunk bounds one PUT body, so a first push of a huge transcript lands in
// pieces that each fit the HTTP timeout and the proxy's body limit.
var maxChunk = 32 << 20

func firstChunk(body []byte, max int) []byte {
	if len(body) <= max {
		return body
	}
	if i := bytes.LastIndexByte(body[:max], '\n'); i >= 0 {
		return body[:i+1]
	}
	return body[:bytes.IndexByte(body, '\n')+1]
}

func decide(entry *wire.Source, head string, redacted []byte) (body []byte, offset int64, noop bool) {
	if entry == nil || entry.HeadHash != head || int64(len(redacted)) < entry.Size {
		return redacted, 0, false
	}
	if int64(len(redacted)) == entry.Size {
		return nil, 0, true
	}
	cut, onBoundary := cutAtSize(redacted, entry.Size)
	if !onBoundary {
		// The server's Size falls inside a line our redaction re-encoded;
		// there is no safe seam, so replace.
		return redacted, 0, false
	}
	return redacted[cut:], entry.Size, false
}

func rawEndOf(rawEnds []int64, redacted []byte, start int64, body []byte) int64 {
	lines := bytes.Count(redacted[:start], []byte{'\n'}) + bytes.Count(body, []byte{'\n'})
	return rawEnds[lines-1]
}

// putSource sends one PUT and decodes the updated Source just to validate the
// 200 body. rawEnd is the offset in the client's raw file the body ends at;
// a server that predates raw_end ignores it. A 409 is not an error: the
// Conflict body comes back so the caller can re-decide.
func putSource(ctx context.Context, cfg config.Client, hc *http.Client, f fileRef, srcID string, offset int64, head string, body []byte, rawEnd int64) (*wire.Conflict, error) {
	q := url.Values{
		"offset":      {strconv.FormatInt(offset, 10)},
		"head_hash":   {head},
		"source_name": {f.sourceName},
		"path":        {f.path},
		"raw_end":     {strconv.FormatInt(rawEnd, 10)},
	}
	u := strings.TrimSuffix(cfg.Server, "/") + wire.RawPathPrefix +
		url.PathEscape(cfg.Machine) + "/" + url.PathEscape(f.harness) + "/" + srcID + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusConflict:
		var c wire.Conflict
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
			return nil, fmt.Errorf("put %s: 409 body: %w", srcID, err)
		}
		return &c, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("put %s: %s: %s", srcID, resp.Status, readErrBody(resp.Body))
	}
	var src wire.Source
	if err := json.NewDecoder(resp.Body).Decode(&src); err != nil {
		return nil, fmt.Errorf("put %s: response: %w", srcID, err)
	}
	return nil, nil
}

func readErrBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 512))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// scan lists every file the globs match — absolute, regular, deduped — with
// the first source that matches a path owning it. A source whose glob cannot
// be evaluated matches nothing this pass.
func scan(sources []config.SourceGlob) []fileRef {
	seen := make(map[string]bool)
	var files []fileRef
	for _, src := range sources {
		matches, err := doublestar.FilepathGlob(src.Glob)
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, m := range matches {
			abs, err := filepath.Abs(m)
			if err != nil || seen[abs] {
				continue
			}
			info, err := os.Stat(abs)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			seen[abs] = true
			files = append(files, fileRef{path: abs, harness: src.Harness, sourceName: src.SourceName, mtimeMs: info.ModTime().UnixMilli(), size: info.Size()})
		}
	}
	return files
}

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

// skewMs is the mtime-vs-UpdatedMs margin the skip needs, because the client
// and server clocks need not agree.
const skewMs = 5 * 60 * 1000

type fileRef struct {
	path       string
	harness    string
	sourceName string
	mtimeMs    int64
	size       int64
	head       *rawHead
}

func Run(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client) (Stats, error) {
	manifest, err := fetchManifest(ctx, cfg, hc)
	if err != nil {
		return Stats{}, err
	}
	var st Stats
	for _, f := range scan(cfg.Sources) {
		st.Scanned++
		if _, _, err := syncSource(ctx, cfg, red, hc, manifest[wire.SrcID(f.path)], f, &st); err != nil {
			st.Failed++
		}
	}
	return st, nil
}

func (s Stats) String() string {
	return fmt.Sprintf("scanned %d, appended %d, replaced %d, unchanged %d, failed %d, sent %d bytes",
		s.Scanned, s.Appended, s.Replaced, s.Unchanged, s.Failed, s.Bytes)
}

func fetchManifest(ctx context.Context, cfg config.Client, hc *http.Client) (map[string]*wire.Source, error) {
	u := strings.TrimSuffix(cfg.Server, "/") + wire.ManifestPath + "?" + url.Values{"machine": {cfg.Machine}}.Encode()
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

// syncSource returns the server's state for the source as its last answer
// reported it (nil when it sent nothing) and whether any answer was a 409.
func syncSource(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client, entry *wire.Source, f fileRef, st *Stats) (held *wire.Source, conflicted bool, err error) {
	srcID := wire.SrcID(f.path)
	if entry != nil && entry.RawEnd > 0 && f.size == entry.RawEnd {
		st.Unchanged++
		return nil, false, nil
	}
	// The server stamps UpdatedMs after it received the bytes, so a file whose
	// last write is older than that stamp (with a 5-minute margin for
	// client/server clock skew) has nothing new; skip it unread. Entries with
	// RawEnd 0 — the legacy shape — are saved the read by this skip alone.
	if entry != nil && f.mtimeMs < entry.UpdatedMs-skewMs {
		st.Unchanged++
		return nil, false, nil
	}
	legacy := entry != nil && entry.RawEnd == 0
	sent, replaced := false, false
	if entry != nil && entry.RawEnd > 0 && entry.Size > 0 {
		tail, err := syncTail(ctx, cfg, red, hc, f, srcID, entry, st)
		held, conflicted = tail.held, tail.conflicted
		if err != nil {
			return held, conflicted, err
		}
		if tail.fallback == nil {
			if tail.sent {
				st.Appended++
			} else {
				st.Unchanged++
			}
			return held, conflicted, nil
		}
		entry, sent = tail.fallback, tail.sent
	}
	file, err := redactComplete(f.path, red)
	if err != nil {
		return held, conflicted, err
	}
	if file == nil {
		if !sent {
			st.Unchanged++
		}
		return held, conflicted, nil
	}
	for conflicts := 0; ; {
		body, offset, noop := decide(entry, file.head, file.body)
		if noop {
			// A legacy entry (RawEnd 0) whose redacted bytes the server already
			// holds in full would read whole every pass forever: one empty
			// append at Size records raw_end so the next pass takes the exact
			// skip. Best effort — an old server answers 400 and a stale one
			// 409, and either way the file just stays on this full path.
			if legacy && !sent {
				if now, c, err := putSource(ctx, cfg, hc, f, srcID, entry.Size, file.head, nil, file.rawEnds[len(file.rawEnds)-1]); err == nil {
					held, conflicted = now, conflicted || c
				}
			}
			switch {
			case !sent:
				st.Unchanged++
			case replaced:
				st.Replaced++
			default:
				st.Appended++
			}
			return held, conflicted, nil
		}
		body = firstChunk(body, maxChunk)
		now, c, err := putSource(ctx, cfg, hc, f, srcID, offset, file.head, body, rawEndOf(file.rawEnds, file.body, offset, body))
		if err != nil {
			return held, conflicted, err
		}
		held = now
		if c {
			conflicted = true
			if conflicts++; conflicts > 1 {
				return held, conflicted, fmt.Errorf("put %s: conflict again at size %d", f.path, now.Size)
			}
			entry = &wire.Source{SrcID: srcID, Size: now.Size, HeadHash: now.HeadHash}
			continue
		}
		st.Bytes += int64(len(body))
		sent, replaced = true, replaced || offset == 0
		entry = &wire.Source{SrcID: srcID, Size: offset + int64(len(body)), HeadHash: file.head}
	}
}

type tailResult struct {
	sent, conflicted bool
	fallback         *wire.Source
	held             *wire.Source
}

func syncTail(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client, f fileRef, srcID string, entry *wire.Source, st *Stats) (tailResult, error) {
	tail, err := redactTail(f.path, red, entry, f.head)
	if err != nil {
		return tailResult{}, err
	}
	if tail == nil {
		return tailResult{fallback: entry}, nil
	}
	var res tailResult
	body, rawEnds := tail.body, tail.rawEnds
	for len(body) > 0 {
		chunk := firstChunk(body, maxChunk)
		lines := bytes.Count(chunk, []byte{'\n'})
		rawEnd := rawEnds[lines-1]
		now, conflicted, err := putSource(ctx, cfg, hc, f, srcID, entry.Size, tail.head, chunk, rawEnd)
		if err != nil {
			return res, err
		}
		res.held = now
		if conflicted {
			res.fallback, res.conflicted = now, true
			return res, nil
		}
		st.Bytes += int64(len(chunk))
		res.sent = true
		body, rawEnds = body[len(chunk):], rawEnds[lines:]
		entry = &wire.Source{SrcID: srcID, Size: entry.Size + int64(len(chunk)), HeadHash: tail.head, RawEnd: rawEnd}
	}
	return res, nil
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

// putSource returns the server's state for the source: the 200 body, or on a
// 409 the Conflict's Size, HeadHash and RawEnd with conflicted set.
func putSource(ctx context.Context, cfg config.Client, hc *http.Client, f fileRef, srcID string, offset int64, head string, body []byte, rawEnd int64) (now *wire.Source, conflicted bool, err error) {
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
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var s wire.Source
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
			return nil, false, fmt.Errorf("put %s: 200 body: %w", srcID, err)
		}
		_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return &s, false, err
	case http.StatusConflict:
		var c wire.Conflict
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
			return nil, false, fmt.Errorf("put %s: 409 body: %w", srcID, err)
		}
		return &wire.Source{SrcID: srcID, Size: c.Size, HeadHash: c.HeadHash, RawEnd: c.RawEnd}, true, nil
	default:
		return nil, false, fmt.Errorf("put %s: %s: %s", srcID, resp.Status, readErrBody(resp.Body))
	}
}

func readErrBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 512))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

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
			files = append(files, fileRef{path: abs, harness: src.Harness, sourceName: src.SourceName, mtimeMs: info.ModTime().UnixMilli(), size: info.Size(), head: new(rawHead)})
		}
	}
	return files
}

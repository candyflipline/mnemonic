// Package push is the client side of the wire protocol: scan the configured
// globs, diff against the server's manifest, send only whole redacted lines.
// Owned by lane push.
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

// Redactor is the part of *redact.Redactor a pass needs. It is an interface
// rather than the concrete type so the pass — and its tests — do not depend on
// internal/redact's rules landing first.
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

// fileRef is one matched file and the source it belongs to. mtimeMs rides
// along from the scan's stat so the skip costs no second stat.
type fileRef struct {
	path       string
	harness    string
	sourceName string
	mtimeMs    int64
}

// Run does one pass. Per source: head hash equal and the redacted size grew
// means append from the server's Size; otherwise send the whole file with
// offset 0. A trailing line without '\n' waits for the next pass. A 409
// re-reads the manifest once for that source. Rerunning with no new bytes
// sends nothing.
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

// fetchManifest maps the server's manifest by SrcID. The author comes from the
// token, not from us.
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

// syncSource diffs one file against its manifest entry and sends what is
// missing. A 409 means our manifest view was stale; the Conflict body carries
// the server's Size and head hash, and exactly one retry re-decides from it.
func syncSource(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client, manifest map[string]*wire.Source, f fileRef, st *Stats) error {
	srcID := wire.SrcID(f.path)
	// The server stamps UpdatedMs after it received the bytes, so a file whose
	// last write is older than that stamp (with a 5-minute margin for
	// client/server clock skew) has nothing new; skip it unread.
	if entry := manifest[srcID]; entry != nil && f.mtimeMs < entry.UpdatedMs-skewMs {
		st.Unchanged++
		return nil
	}
	redacted, head, ok, err := redactComplete(f.path, red)
	if err != nil {
		return err
	}
	if !ok {
		// No whole line yet: nothing to send and nothing to compare.
		st.Unchanged++
		return nil
	}
	entry := manifest[srcID]
	sent, replaced := false, false
	for conflicts := 0; ; {
		body, offset, noop := decide(entry, head, redacted)
		if noop {
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
		conflict, err := putSource(ctx, cfg, hc, f, srcID, offset, head, body)
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

// maxChunk bounds one PUT body, so a first push of a huge transcript lands in
// pieces that each fit the HTTP timeout and the proxy's body limit.
var maxChunk = 32 << 20

// firstChunk is the longest prefix of whole lines within max bytes, or the
// first line alone when that line is longer than max.
func firstChunk(body []byte, max int) []byte {
	if len(body) <= max {
		return body
	}
	if i := bytes.LastIndexByte(body[:max], '\n'); i >= 0 {
		return body[:i+1]
	}
	return body[:bytes.IndexByte(body, '\n')+1]
}

// decide picks the PUT for one attempt: whole at offset 0 whenever we cannot
// prove the server holds our exact redacted prefix, append from Size when it
// does and the file grew, nothing when it holds all of it.
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

// putSource sends one PUT and decodes the updated Source just to validate the
// 200 body. A 409 is not an error: the Conflict body comes back so the caller
// can re-decide.
func putSource(ctx context.Context, cfg config.Client, hc *http.Client, f fileRef, srcID string, offset int64, head string, body []byte) (*wire.Conflict, error) {
	q := url.Values{
		"offset":      {strconv.FormatInt(offset, 10)},
		"head_hash":   {head},
		"source_name": {f.sourceName},
		"path":        {f.path},
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

// readErrBody caps an error body so a chatty server cannot blow up a log line.
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
			files = append(files, fileRef{path: abs, harness: src.Harness, sourceName: src.SourceName, mtimeMs: info.ModTime().UnixMilli()})
		}
	}
	return files
}

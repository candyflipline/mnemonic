package index

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

// TestIngestPlainReportsToolRecords pins local mode's contract: an IngestPlain
// over a data dir reports the records the tools write through the store under
// <dataDir>/raw, so an annotation is searchable after the write itself and not
// only after the next harness file change.
func TestIngestPlainReportsToolRecords(t *testing.T) {
	dataDir := t.TempDir()
	const session = "7c666c01-d38e-4658-8650-854ffb5b626e"
	globs := []config.SourceGlob{{
		Harness:    "claude-code",
		SourceName: "claude",
		Glob:       filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "session.jsonl"),
	}}
	const author, machine = "tester", "testbox"
	ix, err := Open(filepath.Join(dataDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	st, err := ix.IngestPlain(globs, author, machine, IngestOptions{SettleAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if st[Fresh] != 1 {
		t.Fatalf("first ingest: fresh=%d unchanged=%d, want fresh=1 (the session file)", st[Fresh], st[Unchanged])
	}

	// The record annotate appends: one schema.Annotation line through the
	// store under the data dir, the write the local MCP ctx carries.
	line, err := json.Marshal(schema.Annotation{
		ID:     "0123456789abcdef0123456789abcdef",
		Target: schema.Target{SessionID: session},
		Name:   "grade",
		Value:  schema.Label("pass"),
		Source: schema.SourceHuman,
		TS:     time.Date(2026, 1, 19, 16, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	stStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer stStore.Close()
	if _, duplicate, err := stStore.Append(store.Today(author, schema.AnnotationHarness, ""), "", append(line, '\n')); err != nil || duplicate {
		t.Fatalf("append annotation: duplicate=%v err=%v", duplicate, err)
	}

	st, err = ix.IngestPlain(globs, author, machine, IngestOptions{SettleAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if st[Fresh] != 1 || st[Unchanged] != 1 {
		t.Fatalf("second ingest: fresh=%d unchanged=%d, want fresh=1 (the annotation source) unchanged=1 (the session file)", st[Fresh], st[Unchanged])
	}

	db, release := ix.Snapshot()
	defer release()
	notes := db.sessionAnnotations(session)
	if len(notes) != 1 {
		t.Fatalf("sessionAnnotations(%s): %d notes, want 1", session, len(notes))
	}
	if notes[0].Name != "grade" || notes[0].ID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("annotation is %s=%v id %s, want grade id 0123456789abcdef0123456789abcdef", notes[0].Name, notes[0].Value, notes[0].ID)
	}
	if label, ok := notes[0].Value.Label(); !ok || label != "pass" {
		t.Fatalf("annotation value is %v, want label pass", notes[0].Value)
	}
}

// TestAnnotateStoresOnlyRedactedText pins the write path's redaction: the
// annotation's user text goes through the redactor push uploads with before
// the record is appended, so raw/ holds only redacted text on every write
// path; the result text carries the redacted value, and a retried identical
// call is the duplicate of the stored (redacted) record.
func TestAnnotateStoresOnlyRedactedText(t *testing.T) {
	db, dir := newQueryDB(t)
	const session = "7c666c01-d38e-4658-8650-854ffb5b626e"
	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	const ghToken = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef1234"
	args, err := json.Marshal(map[string]any{
		"session_id": formatSessionID(session),
		"name":       "deploy-note",
		"text":       "Deploy with " + awsKey + " using " + ghToken + " today.",
	})
	if err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(map[string]auth.Token{
		auth.Hash("tok-a"): {Author: "a", Scopes: []auth.Scope{auth.Read, auth.Annotate}},
	})
	ctl, err := LoadControls(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, ok := tokens.Context(WithControls(context.Background(), ctl), "tok-a")
	if !ok {
		t.Fatal("token")
	}
	stc, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stc.Close()
	ctx = store.WithStore(ctx, stc)

	data, text, err := Call(ctx, db, "annotate", args)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"duplicate":false`)) {
		t.Fatalf("first annotate is not fresh: %s", data)
	}
	for _, secret := range []string{awsKey, ghToken} {
		if bytes.Contains(data, []byte(secret)) || bytes.Contains([]byte(text), []byte(secret)) {
			t.Fatalf("annotate result carries the secret %q:\n%s\n%s", secret, data, text)
		}
	}

	// The record the write appended under the data dir.
	var stored []byte
	root := filepath.Join(dir, "raw", "author=a", "machine="+store.ServerMachine, "harness="+schema.AnnotationHarness)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl.zst") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dec, err := zstd.NewReader(bytes.NewReader(b))
		if err != nil {
			return err
		}
		defer dec.Close()
		line, err := io.ReadAll(dec)
		if err != nil {
			return err
		}
		stored = append(stored, line...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored, []byte(`"name":"deploy-note"`)) {
		t.Fatalf("the annotation record is missing:\n%s", stored)
	}
	for _, secret := range []string{awsKey, ghToken} {
		if bytes.Contains(stored, []byte(secret)) {
			t.Fatalf("raw/ holds the secret %q:\n%s", secret, stored)
		}
	}
	for _, marker := range []string{"[REDACTED:aws-access-token]", "[REDACTED:github-token]"} {
		if !bytes.Contains(stored, []byte(marker)) {
			t.Fatalf("the record lost the %s redaction:\n%s", marker, stored)
		}
	}

	// The same call again is the duplicate of the stored record.
	_, text, err = Call(ctx, db, "annotate", args)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(text), []byte("Already annotated")) {
		t.Fatalf("the retried call is not a duplicate: %s", text)
	}
}

// pushRawCopy writes the raw copy main's old `mnemonic local` pushed: the
// lines compressed as one zstd frame under <dataDir>/raw's author/machine/
// harness layout, next to the manifest naming the harness file it came from.
func pushRawCopy(t *testing.T, dataDir, author, machine, harness, sourceName, path string, lines []byte) {
	t.Helper()
	var frame bytes.Buffer
	w, err := zstd.NewWriter(&frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(lines); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	first, _, _ := bytes.Cut(lines, []byte{'\n'})
	m := struct {
		wire.Source
		ZstSize int64 `json:"zst_size"`
	}{Source: wire.Source{
		SrcID: wire.SrcID(path), Harness: harness, SourceName: sourceName, Path: path,
		Size: int64(len(lines)), HeadHash: wire.HeadHash(first), UpdatedMs: time.Now().UnixMilli(),
	}, ZstSize: int64(frame.Len())}
	key := layout.Key{Author: author, Machine: machine, Harness: harness, SrcID: m.SrcID}
	if err := os.MkdirAll(filepath.Dir(key.Raw(dataDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key.Raw(dataDir), frame.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key.Manifest(dataDir), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// plainSession writes a two-line claude-code session whose user text is text.
func plainSession(t *testing.T, path, session, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/demo","uuid":"u1","timestamp":"2026-01-19T15:58:40.000Z","message":{"role":"user","content":%q}}`, session, text),
		fmt.Sprintf(`{"type":"assistant","sessionId":%q,"cwd":"/work/demo","uuid":"a1","parentUuid":"u1","requestId":"req-1","timestamp":"2026-01-19T15:58:41.421Z","message":{"model":"claude-opus-4-5-20251101","role":"assistant","usage":{"input_tokens":9,"output_tokens":5},"content":[{"type":"text","text":"done."}]}}`, session),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func searchCount(t *testing.T, db *DB, word string) int {
	t.Helper()
	data, _, err := Call(context.Background(), db, "search_sessions", json.RawMessage(`{"query":"`+word+`"}`))
	if err != nil {
		t.Fatalf("search_sessions %q: %v", word, err)
	}
	var out struct {
		ResultCount int `json:"result_count"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("search_sessions %q: %v: %s", word, err, data)
	}
	return out.ResultCount
}

// TestIngestPlainOneCheckpointPerKey pins local mode's key ownership: a raw
// copy old `mnemonic local` pushed under the same key as a live harness file
// is dropped (the plain file wins, one checkpoint, nothing on disk deleted),
// a raw copy whose plain file no longer exists is still ingested (it is the
// only copy left), and ingest() refuses a list holding one key twice, so the
// duplicate cannot come back by another path.
func TestIngestPlainOneCheckpointPerKey(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	files := filepath.Join(dir, "harness")
	const author, machine, harness = "tester", "testbox", "claude-code"
	const sessionA = "11111111-4658-8650-854f-0c666c01d38e"
	const sessionB = "22222222-4658-8650-854f-0c666c01d38e"
	const sessionC = "33333333-4658-8650-854f-0c666c01d38e"
	pathA := filepath.Join(files, "a.jsonl")
	pathB := filepath.Join(files, "b.jsonl")
	pathC := filepath.Join(files, "c.jsonl")
	plainSession(t, pathA, sessionA, "quorum stands.")
	plainSession(t, pathB, sessionB, "harbor stands.")
	plainSession(t, pathC, sessionC, "lumen stands.")
	// Old local pushed a and b; the raw copy of a diverged from the live file
	// (its last append never happened locally), and retention deleted b.
	pushRawCopy(t, dataDir, author, machine, harness, "claude", pathA,
		[]byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/demo","uuid":"u0","timestamp":"2026-01-19T15:58:39.000Z","message":{"role":"user","content":"rawtwin lingers."}}`+"\n", sessionA)))
	pushRawCopy(t, dataDir, author, machine, harness, "claude", pathB, mustRead(t, pathB))
	if err := os.Remove(pathB); err != nil {
		t.Fatal(err)
	}
	globs := []config.SourceGlob{
		{Harness: harness, SourceName: "claude", Glob: pathA},
		{Harness: harness, SourceName: "claude", Glob: pathB},
		{Harness: harness, SourceName: "claude", Glob: pathC},
	}
	ix, err := Open(filepath.Join(dataDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	st, err := ix.IngestPlain(globs, author, machine, IngestOptions{SettleAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if st[Fresh] != 3 {
		t.Fatalf("lap 0: fresh=%d unchanged=%d, want fresh=3 (a's plain file, b's raw copy, c's plain file)", st[Fresh], st[Unchanged])
	}
	for lap := 1; lap <= 3; lap++ {
		st, err = ix.IngestPlain(globs, author, machine, IngestOptions{SettleAll: true})
		if err != nil {
			t.Fatalf("lap %d: %v", lap, err)
		}
		if st[Unchanged] != 3 {
			t.Fatalf("lap %d: unchanged=%d fresh=%d resumed=%d reparsed=%d, want unchanged=3", lap, st[Unchanged], st[Fresh], st[Resumed], st[Reparsed])
		}
	}

	// One checkpoint per key, and a's is the plain file's.
	store := filepath.Join(dataDir, "index")
	ents, err := os.ReadDir(srcDir(store))
	if err != nil {
		t.Fatal(err)
	}
	ckpts := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ckpt") {
			ckpts++
		}
	}
	if ckpts != 3 {
		t.Fatalf("src/ holds %d checkpoints, want 3 (one per key)", ckpts)
	}
	c, ok := readCkpt(store, sourceKey(layout.Key{Author: author, Machine: machine, Harness: harness, SrcID: wire.SrcID(pathA)}))
	if !ok || c.Kind != KindPlain {
		t.Fatalf("a's checkpoint: kind=%d ok=%v, want KindPlain: the live file, not the raw copy, owns the key", c.Kind, ok)
	}

	// All three sources answer search: a and c from their plain files, b from
	// its raw copy, and the raw copy's diverged text stays out.
	db, release := ix.Snapshot()
	defer release()
	for word, want := range map[string]int{"quorum": 1, "harbor": 1, "lumen": 1, "rawtwin": 0} {
		if got := searchCount(t, db, word); got != want {
			t.Fatalf("search %q: result_count=%d, want %d", word, got, want)
		}
	}

	// A list holding one key twice is refused, naming the key.
	dup := source{kind: KindStore, mpath: pathA, key: layout.Key{Author: author, Machine: machine, Harness: harness, SrcID: wire.SrcID(pathA)}}
	_, err = ingest(dataDir, store, []source{dup, dup}, time.Hour, true, nil)
	if err == nil || !strings.Contains(err.Error(), sourceKey(dup.key)) {
		t.Fatalf("ingest with a duplicate key: err=%v, want it to name %s", err, sourceKey(dup.key))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIngestPlainRedactsAtIngest pins local mode's redaction contract: every
// line a plain source's parse reads goes through the redactor push uploads
// with, so the index holds the redacted text and neither the event text, a
// search term, a like filter nor lower() can return the secret. The file on
// disk keeps it, and the checkpoint stays in the plain file's bytes.
func TestIngestPlainRedactsAtIngest(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "session.jsonl")
	const session = "7c666c01-d38e-4658-8650-854ffb5b626e"
	const awsKey = "AKIAIOSFODNN7EXAMPLE"
	pem, err := json.Marshal("-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0Z3VS5JJcds3xfn0GByhQmluD9o1Pw\n-----END RSA PRIVATE KEY-----")
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/work/demo","uuid":"u1","timestamp":"2026-01-19T15:58:40.000Z","message":{"role":"user","content":"Deploy with %s please."}}`, session, awsKey),
		fmt.Sprintf(`{"type":"assistant","sessionId":%q,"cwd":"/work/demo","uuid":"a1","parentUuid":"u1","requestId":"req-1","timestamp":"2026-01-19T15:58:41.421Z","message":{"model":"claude-opus-4-5-20251101","role":"assistant","usage":{"input_tokens":9,"output_tokens":5},"content":[{"type":"text","text":%s}]}}`, session, pem),
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	red, err := redact.New(nil) // as push builds it: no config file, no extra patterns
	if err != nil {
		t.Fatal(err)
	}
	globs := []config.SourceGlob{{Harness: "claude-code", SourceName: "claude", Glob: file}}
	ix, err := Open(filepath.Join(dir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	st, err := ix.IngestPlain(globs, "tester", "testbox", IngestOptions{SettleAll: true, Red: red})
	if err != nil {
		t.Fatal(err)
	}
	if st[Fresh] != 1 {
		t.Fatalf("ingest: fresh=%d, want fresh=1", st[Fresh])
	}

	// The file on disk is never touched.
	onDisk, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(onDisk, []byte(awsKey)) {
		t.Fatalf("the plain file lost the secret; local mode must not touch it")
	}

	db, release := ix.Snapshot()
	defer release()
	ctx := context.Background()
	openID := base64.RawURLEncoding.EncodeToString([]byte(session))
	probes := []struct {
		name string
		args string
		echo bool // the prose repeats the caller's own query; only data is index content
	}{
		{"open", `{"id":"session:` + openID + `","limit":10}`, false},
		{"get_trace", `{"session_id":"session:` + openID + `"}`, false},
		{"search_sessions", `{"query":"AKIAIOSFODNN7EXAMPLE"}`, true},
		{"search_sessions", `{"query":"akiaiosfodnn7example"}`, true},
		{"search_sessions", `{"query":"MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn"}`, true},
		{"query", `{"query":"from: events | select: text_content, lower(text_content)","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z"}`, false},
		{"query", `{"query":"from: events | filter: text_content like '%AKIAIOSF%' | select: event_uid","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z"}`, true},
		{"query", `{"query":"from: events | filter: lower(text_content) like '%akiaiosf%' | select: event_uid","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z"}`, true},
		{"query", `{"query":"from: events | filter: payload_json like '%AKIAIOSF%' | select: event_uid","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z"}`, true},
		{"query", `{"query":"from: events | filter: payload_json like '%MIIEowIBAAKCAQEA%' | select: event_uid","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z"}`, true},
	}
	for _, p := range probes {
		data, text, err := Call(ctx, db, p.name, json.RawMessage(p.args))
		if err != nil {
			t.Fatalf("%s %s: %v", p.name, p.args, err)
		}
		if p.name == "search_sessions" && !bytes.Contains(data, []byte(`"result_count":0`)) {
			t.Fatalf("%s %s: search found the secret: %s", p.name, p.args, data)
		}
		for _, secret := range []string{awsKey, "akiaiosfodnn7example", "BEGIN RSA PRIVATE KEY", "MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn"} {
			if bytes.Contains(data, []byte(secret)) || (!p.echo && bytes.Contains([]byte(text), []byte(secret))) {
				t.Fatalf("%s %s: response holds the secret %q:\n%s\n%s", p.name, p.args, secret, data, text)
			}
		}
		// The redacted event must still be there: open answers the session and
		// the query sees its text with the marker in place.
		if p.name == "open" && !bytes.Contains(data, []byte("[REDACTED:aws-access-token]")) {
			t.Fatalf("open: the redacted text is missing:\n%s", data)
		}
	}
}

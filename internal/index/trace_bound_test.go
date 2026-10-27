package index

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
)

// TestTraceBudgetHoldsNoSpansDecode puts a huge session H and a small session
// S in one settled part, at two sizes of H (5k and 20k tool pairs, about 4 and
// 17 MiB of spans JSON), with the get_trace budget lowered to 1 MiB so H is
// over it. It checks that get_trace refuses H before any spans decode, that
// the heap the refusal adds does not grow with H, that S answers as before,
// that a caller who cannot open H gets "session not found" and not the size,
// and that annotate's span check on H finds a late span, misses an absent
// one, and holds a heap that does not grow with H.
func TestTraceBudgetHoldsNoSpansDecode(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 40k rows")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(25))
	defer func(b int) { traceBudget = b }(traceBudget)
	traceBudget = 1 << 20

	small := traceBoundRun(t, 5_000)
	large := traceBoundRun(t, 20_000)
	t.Logf("get_trace refusal adds %d B at %d spans bytes, %d B at %d", small.refuseGrowth, small.spansBytes, large.refuseGrowth, large.spansBytes)
	t.Logf("annotate span scan adds %d B at %d spans bytes, %d B at %d", small.scanGrowth, small.spansBytes, large.scanGrowth, large.spansBytes)
	const bound = 4 << 20
	if d := large.refuseGrowth - small.refuseGrowth; d > bound {
		t.Errorf("the refusal's heap grew %d B with the session, want <= %d", d, bound)
	}
	if d := large.scanGrowth - small.scanGrowth; d > bound {
		t.Errorf("annotate's span scan heap grew %d B with the session, want <= %d", d, bound)
	}
}

type traceBound struct {
	spansBytes               int
	refuseGrowth, scanGrowth int64
}

func traceBoundRun(t *testing.T, pairs int) traceBound {
	t.Helper()
	root, dir := t.TempDir(), t.TempDir()
	shard := func(sid string) uint32 {
		h := fnv.New32a()
		h.Write([]byte(sid))
		return h.Sum32() % partShards
	}
	const hid = "00000000-0000-4000-8000-0000000000a1"
	sid := ""
	for i := 2; ; i++ {
		if s := fmt.Sprintf("00000000-0000-4000-8000-%012d", i); shard(s) == shard(hid) {
			sid = s
			break
		}
	}
	writeRawSource(t, root, "h", "m", "claude-code", hid, "/p/"+hid+".jsonl", claudeToolRows(hid, pairs), 1000)
	writeRawSource(t, root, "h", "m", "claude-code", sid, "/p/"+sid+".jsonl", claudeToolRows(sid, 2), 1000)
	if _, err := Ingest(root, dir, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if n := len(mustManifest(t, dir).Parts); n != 1 {
		t.Fatalf("%d parts after the settle, want 1", n)
	}
	var out traceBound
	{
		db, err := OpenDB(dir)
		if err != nil {
			t.Fatal(err)
		}
		out.spansBytes = db.sessionSpansSize(hid)
		spans := db.sessionSpans(hid)
		if len(spans) != 2*pairs+2 {
			t.Fatalf("H has %d spans, want %d", len(spans), 2*pairs+2)
		}
		late := spans[len(spans)-1].SpanID
		db.Close()
		if out.spansBytes <= traceBudget {
			t.Fatalf("H is %d B of spans, not over the %d B budget", out.spansBytes, traceBudget)
		}
		out.refuseGrowth, out.scanGrowth = traceBoundCalls(t, dir, hid, sid, late, out.spansBytes)
	}
	return out
}

func traceBoundCalls(t *testing.T, dir, hid, sid, late string, spansBytes int) (refuseGrowth, scanGrowth int64) {
	t.Helper()
	ctl, err := LoadControls(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.NewKeys(map[string]auth.Grant{
		"k-h": {Author: "h", Scopes: []auth.Scope{auth.Read, auth.Annotate}},
		"k-z": {Tenant: "other", Author: "z", Scopes: []auth.Scope{auth.Read}},
	})
	base := WithControls(context.Background(), ctl)
	idH, _ := keys.Identify("k-h")
	ctxH := auth.NewContext(base, idH)
	stc, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stc.Close()
	ctxH = store.WithStore(ctxH, stc)
	idZ, _ := keys.Identify("k-z")
	ctxZ := auth.NewContext(base, idZ)
	args := func(session string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"session_id": formatSessionID(session)})
		return b
	}

	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var refusal error
	refuseGrowth = peakAbove(func() {
		_, _, refusal = Call(ctxH, db, "get_trace", args(hid))
	})
	mb := (spansBytes + 1<<20 - 1) >> 20
	want := fmt.Sprintf("trace too large for get_trace: %d MB of spans (limit 1 MB); export it with: mnemonic export otlp --data-dir <data dir> --session %s", mb, formatSessionID(hid))
	if refusal == nil || refusal.Error() != want {
		t.Errorf("get_trace on H: %v, want %q", refusal, want)
	}

	_, text, err := Call(ctxH, db, "get_trace", args(sid))
	if err != nil || !strings.HasPrefix(text, "Trace "+formatSessionID(sid)+": 6 span(s), tokens in 6 / out 4") {
		t.Errorf("get_trace on S: %v %q, want a 6 span trace", err, text)
	}

	if _, _, err := Call(ctxZ, db, "get_trace", args(hid)); err == nil || err.Error() != "session not found" {
		t.Errorf("get_trace on H by a caller outside its tenant: %v, want session not found", err)
	}

	scanGrowth = peakAbove(func() {
		if err := requireTarget(ctxH, db, hid, late); err != nil {
			t.Errorf("late span of H: %v, want found", err)
		}
	})
	if err := requireTarget(ctxH, db, hid, "no-such-span"); err == nil || err.Error() != "span not found in the session" {
		t.Errorf("absent span of H: %v, want span not found in the session", err)
	}
	annotate, _ := json.Marshal(map[string]any{"session_id": formatSessionID(hid), "span_id": late, "name": "n", "text": "t"})
	if _, _, err := Call(ctxH, db, "annotate", annotate); err != nil {
		t.Errorf("annotate the late span of H: %v", err)
	}
	return refuseGrowth, scanGrowth
}

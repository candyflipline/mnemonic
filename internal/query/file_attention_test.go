package query

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Ports of the resolve_tail, build_data and format_text tests in upstream
// upstream mcp-core/src/file_attention_v1.rs.

func TestFaRejectsNegativeLimit(t *testing.T) {
	raw := json.RawMessage(`{"path": "crates/foo/tee.rs", "limit": -1}`)
	if _, err := faParseArgs(raw); err == nil || err.Error() != "limit must be between 1 and 25" {
		t.Fatalf("negative limit: got %v", err)
	}
}

func TestFaStripRootRemovesPrefixWithOneSlash(t *testing.T) {
	if rel, ok := faStripRoot("/a/b/c/crates/x/file.rs", "/a/b/c"); !ok || rel != "crates/x/file.rs" {
		t.Fatalf("strip: %q %v", rel, ok)
	}
	if rel, ok := faStripRoot("/a/b/c/crates/x/file.rs", "/a/b/c/"); !ok || rel != "crates/x/file.rs" {
		t.Fatalf("strip trailing slash: %q %v", rel, ok)
	}
	if _, ok := faStripRoot("/other/crates/x/file.rs", "/a/b/c"); ok {
		t.Fatal("unrelated prefix must not strip")
	}
	if _, ok := faStripRoot("/x", ""); ok {
		t.Fatal("empty root must not strip")
	}
}

func TestFaResolveTailPassesUnscopedRelativePathsThrough(t *testing.T) {
	tail := faResolveTail("crates/foo/tee.rs", "")
	if tail.Rel != "crates/foo/tee.rs" || tail.Abs || tail.DeriveRoots {
		t.Fatalf("tail = %#v", tail)
	}
	tail = faResolveTail("./crates/foo/tee.rs", "")
	if tail.Rel != "crates/foo/tee.rs" || !tail.Normalized {
		t.Fatalf("./ tail = %#v", tail)
	}
}

func TestFaResolveTailNormalizesDotDotAndRepeatedSlashes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"crates/foo/../foo.rs", "crates/foo.rs"},
		{"crates//foo///bar.rs", "crates/foo/bar.rs"},
		{"crates/foo dir/x.rs", "crates/foo dir/x.rs"},
	} {
		tail := faResolveTail(tc.in, "")
		if tail.Rel != tc.want || tail.Abs {
			t.Fatalf("%q -> %q (abs=%v), want %q", tc.in, tail.Rel, tail.Abs, tc.want)
		}
	}
}

func TestFaTailSegmentsCountsNonEmptyComponents(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"mod.rs", 1},
		{"src/lib.rs", 2},
		{"a/b/c.txt", 3},
		{"/a/b", 2},
	} {
		if got := faTailSegments(tc.in); got != tc.want {
			t.Fatalf("faTailSegments(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{"////", "/", ""} {
		if got := faTailSegments(in); got != 0 {
			t.Fatalf("faTailSegments(%q) = %d, want 0", in, got)
		}
	}
}

// faGitDir makes an empty Git repository root (a .git directory, no objects).
func faGitDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFaResolveTailCarriesLaunchProjectIdentity(t *testing.T) {
	root := faGitDir(t)
	tail := faResolveTail("crates/new/file.rs", root)
	if tail.Rel != "crates/new/file.rs" || tail.Root != root || tail.ProjID != qProjectIDForRoot(root) || !tail.DeriveRoots {
		t.Fatalf("tail = %#v", tail)
	}
}

func TestFaResolveTailUsesExactLaunchDirectoryWithoutGit(t *testing.T) {
	launch := t.TempDir()
	tail := faResolveTail("crates/foo/tee.rs", launch)
	if tail.Rel != "crates/foo/tee.rs" || tail.Root != launch || !strings.HasPrefix(tail.ProjID, "dir:") || !tail.DeriveRoots {
		t.Fatalf("tail = %#v", tail)
	}
	escape := faResolveTail("../outside.rs", launch)
	if escape.ProjID != "" || escape.Root != "" || escape.DeriveRoots {
		t.Fatalf("escape = %#v", escape)
	}
}

func TestFaResolveTailMakesNestedLaunchPathsRepositoryRelative(t *testing.T) {
	repo := faGitDir(t)
	nested := filepath.Join(repo, "crates", "foo")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	tail := faResolveTail("src/lib.rs", nested)
	if tail.Rel != "crates/foo/src/lib.rs" {
		t.Fatalf("nested rel = %q", tail.Rel)
	}
	tail = faResolveTail("../shared.rs", nested)
	if tail.Rel != "crates/shared.rs" {
		t.Fatalf("parent rel = %q", tail.Rel)
	}
	tail = faResolveTail("../../../outside.rs", nested)
	if tail.ProjID != "" || tail.DeriveRoots {
		t.Fatalf("outside = %#v", tail)
	}
}

func TestFaResolveTailNeverDerivesProvenanceFromCompoundInput(t *testing.T) {
	root := faGitDir(t)
	for _, delim := range []string{"\x00", "\n", "\r", ";", "|", "&", "`"} {
		tail := faResolveTail("bad"+delim+"command/../src/lib.rs", root)
		if tail.DeriveRoots || tail.Root != "" || tail.ProjID != "" {
			t.Fatalf("delimiter %q: tail = %#v", delim, tail)
		}
	}
}

func TestFaResolveTailStripsAbsolutePathToMarkerRoot(t *testing.T) {
	root := faGitDir(t)
	file := filepath.Join(root, "crates", "foo", "tee.rs")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := faResolveTail(file, "")
	if tail.Rel != "crates/foo/tee.rs" || tail.Abs || tail.Root != root ||
		tail.ProjID != qProjectIDForRoot(root) || !tail.DeriveRoots {
		t.Fatalf("tail = %#v", tail)
	}
}

func TestFaResolveTailClosesProjectScopeForAbsolutePathInAnotherProject(t *testing.T) {
	launch := faGitDir(t)
	other := faGitDir(t)
	file := filepath.Join(other, "src", "lib.rs")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := faResolveTail(file, launch)
	if tail.Rel != "src/lib.rs" || tail.Root != other || tail.ProjID != "" {
		t.Fatalf("tail = %#v", tail)
	}
}

func TestFaResolveTailClosesDirectoryScopeForAbsolutePathOutsideLaunch(t *testing.T) {
	launch := t.TempDir()
	outside := t.TempDir()
	file := filepath.Join(outside, "deep", "file.rs")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := faResolveTail(file, launch)
	if !tail.Abs || tail.Rel != file || tail.ProjID != "" {
		t.Fatalf("tail = %#v", tail)
	}
}

func TestFaResolveTailKeepsUnmarkedLinkedWorktreeRoot(t *testing.T) {
	root := faGitDir(t)
	wtName := filepath.Join(root, ".git", "worktrees", "linked")
	if err := os.MkdirAll(wtName, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtName, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+wtName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	tail := faResolveTail(filepath.Join("src", "lib.rs"), worktree)
	if tail.Root != worktree || tail.ProjID != qProjectIDForRoot(root) {
		t.Fatalf("tail = %#v", tail)
	}
}

func TestFaResolveTailFallsBackToAbsoluteWhenNoMarker(t *testing.T) {
	path := "/nonexistent-xyz/deep/unmarked/file.rs"
	tail := faResolveTail(path, "")
	if !tail.Abs || tail.Rel != path {
		t.Fatalf("tail = %#v", tail)
	}
}

// --- build_data ---

// faTouchFixture builds one newest-first touch like upstream's touch() helper.
func faTouchFixture(session, event, tool, matchKind, root string, ts int64) faTouch {
	var ms *int64
	var turn *uint32
	order := uint64(0)
	if ts > 0 {
		ms = &ts
		seq := uint32(1)
		turn = &seq
		order = uint64(ts)
	}
	matchedPath := ""
	if root != "" {
		matchedPath = root + "/crates/foo/tee.rs"
	}
	return faTouch{
		SessionID: session, EventUID: event, ToolCallID: "call-" + event,
		Harness: "claude-code", SourceName: "claude", ToolName: tool, ToolPhase: "request",
		MatchedPath: matchedPath, MatchKind: matchKind, WorktreeRoot: root,
		EventUnixMs: ms, EventOrder: order, TurnSeq: turn,
		InputPreview: `{"file_path":"crates/foo/tee.rs"}`,
	}
}

func faDataArgs(limit int) *faArgs {
	return &faArgs{Path: "crates/foo/tee.rs", Scope: "all", Granularity: "sessions", Limit: limit}
}

func TestFaBuildDataSurfacesDistinctRootsAndFlagsAmbiguity(t *testing.T) {
	main, sib := "/repos/main", "/repos/sibling"
	touches := []faTouch{
		faTouchFixture("sess-main", "ev-main", "Edit", "path_suffix", main, 3000),
		faTouchFixture("sess-sib", "ev-sib", "Edit", "path_suffix", sib, 2000),
		faTouchFixture("sess-iso", "ev-iso", "Read", "path_suffix", "", 1000),
	}
	warnings := []string{}
	data := faBuildData(faDataArgs(50), &faTail{Rel: "crates/foo/tee.rs"}, touches, &warnings)
	summary := data["summary"].(map[string]any)
	if summary["total_touches"] != 3 || summary["distinct_sessions"] != 3 ||
		summary["distinct_roots"] != 3 || summary["distinct_known_roots"] != 2 ||
		summary["unknown_root_touches"] != 1 || summary["ambiguous"] != true {
		t.Fatalf("summary = %#v", summary)
	}
	roots := data["roots"].([]any)
	// Equal touch counts sort by root ascending, and "(" precedes "/".
	wantRoots := []string{faUnknownRoot, main, sib}
	for i, want := range wantRoots {
		if got := roots[i].(map[string]any)["root"]; got != want {
			t.Fatalf("roots[%d] = %v, want %v", i, got, want)
		}
	}
	sessions := data["sessions"].([]any)
	first := sessions[0].(map[string]any)
	if first["id"] != "session:c2Vzcy1tYWlu" || first["rank"] != 1 {
		t.Fatalf("sessions[0] = %#v", first)
	}
	if first["open"].(map[string]any)["session_id"] != "session:c2Vzcy1tYWlu" {
		t.Fatalf("open = %#v", first["open"])
	}
	payload := map[string]any{"data": data, "warnings": warnings}
	text := faText(payload)
	if !strings.Contains(text, "Ambiguous tail") ||
		!strings.Contains(text, "Worktree-root attribution is incomplete") {
		t.Fatalf("text missing ambiguity lines:\n%s", text)
	}
}

func TestFaBuildDataEventsGranularity(t *testing.T) {
	touches := []faTouch{faTouchFixture("sess-ev", "ev-1", "Edit", "path_suffix", "/repos/main", 1777464001123)}
	turn := uint32(7)
	touches[0].TurnSeq = &turn
	a := faDataArgs(50)
	a.Granularity = "events"
	data := faBuildData(a, &faTail{Rel: "crates/foo/tee.rs"}, touches, &[]string{})
	events := data["events"].([]any)
	item := events[0].(map[string]any)
	event := item["event"].(map[string]any)
	if event["timestamp"] != "2026-04-29T12:00:01.123Z" {
		t.Fatalf("timestamp = %v", event["timestamp"])
	}
	wantTurn := "turn:" + qB64URL([]byte("sess-ev")) + ":7"
	if item["open"].(map[string]any)["turn_id"] != wantTurn {
		t.Fatalf("turn_id = %v", item["open"].(map[string]any)["turn_id"])
	}
	if event["tool_name"] != "Edit" || event["phase"] != "request" {
		t.Fatalf("event = %#v", event)
	}
}

func TestFaBuildDataRespectsDisplayLimitAndMarksTruncated(t *testing.T) {
	touches := make([]faTouch, 0, 5)
	for i := 0; i < 5; i++ {
		touches = append(touches, faTouchFixture(
			"sess-"+string(rune('1'+i)), "ev-"+string(rune('1'+i)), "Edit", "path_suffix", "/repos/main", int64(5000-i*1000)))
	}
	data := faBuildData(faDataArgs(2), &faTail{Rel: "crates/foo/tee.rs"}, touches, &[]string{})
	if data["result_count"] != 2 || data["truncated"] != true {
		t.Fatalf("count/truncated = %v/%v", data["result_count"], data["truncated"])
	}
	summary := data["summary"].(map[string]any)
	if summary["distinct_sessions"] != 5 || summary["total_touches"] != 5 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestFaBuildDataSkippedInvalidIDIsAWarningNotTruncation(t *testing.T) {
	touches := []faTouch{
		faTouchFixture("", "ev-x", "Edit", "path_suffix", "/repos/main", 3000),
		faTouchFixture("sess-ok", "ev-ok", "Edit", "path_suffix", "/repos/main", 2000),
	}
	warnings := []string{}
	data := faBuildData(faDataArgs(50), &faTail{Rel: "crates/foo/tee.rs"}, touches, &warnings)
	if data["truncated"] != false || data["result_count"] != 1 {
		t.Fatalf("truncated/count = %v/%v", data["truncated"], data["result_count"])
	}
	dropped := "dropped 1 touch(es) whose repository identifiers could not be encoded; they are excluded from the result and its counts."
	if strings.Count(strings.Join(warnings, "\n"), dropped) < 1 {
		t.Fatalf("warnings missing dropped notice: %#v", warnings)
	}
}

func TestFaBuildDataUnknownRootBucket(t *testing.T) {
	touches := []faTouch{
		faTouchFixture("sess-a", "ev-a", "Edit", "path_suffix", "/repos/main", 3000),
		faTouchFixture("sess-b", "ev-b", "Edit", "path_suffix", "/repos/main", 2000),
		faTouchFixture("sess-c", "ev-c", "Read", "path_suffix", "", 1000),
	}
	data := faBuildData(faDataArgs(50), &faTail{Rel: "crates/foo/tee.rs"}, touches, &[]string{})
	summary := data["summary"].(map[string]any)
	if summary["distinct_roots"] != 2 || summary["distinct_known_roots"] != 1 ||
		summary["unknown_root_touches"] != 1 || summary["ambiguous"] != true {
		t.Fatalf("summary = %#v", summary)
	}
	roots := data["roots"].([]any)
	found := false
	for _, r := range roots {
		if r.(map[string]any)["root"] == faUnknownRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("roots missing %q: %#v", faUnknownRoot, roots)
	}
}

func TestFaBuildDataSessionTouchedUnderTwoRootsListsBoth(t *testing.T) {
	touches := []faTouch{
		faTouchFixture("sess-multi", "ev-1", "Edit", "path_suffix", "/repos/main", 3000),
		faTouchFixture("sess-multi", "ev-2", "Edit", "path_suffix", "/repos/sibling", 2000),
	}
	data := faBuildData(faDataArgs(50), &faTail{Rel: "crates/foo/tee.rs"}, touches, &[]string{})
	sessions := data["sessions"].([]any)
	session := sessions[0].(map[string]any)["session"].(map[string]any)
	roots := session["worktree_roots"].([]string)
	if len(roots) != 2 || roots[0] != "/repos/main" || roots[1] != "/repos/sibling" {
		t.Fatalf("worktree_roots = %#v", roots)
	}
}

func TestFaBuildDataAllTouchesMissingTimestampsNullSpan(t *testing.T) {
	touches := []faTouch{
		faTouchFixture("sess-null", "ev-n1", "Edit", "path_suffix", "/repos/main", 0),
		faTouchFixture("sess-null", "ev-n2", "Edit", "path_suffix", "/repos/main", 0),
	}
	data := faBuildData(faDataArgs(50), &faTail{Rel: "crates/foo/tee.rs"}, touches, &[]string{})
	summary := data["summary"].(map[string]any)
	if summary["first_touch"] != nil || summary["last_touch"] != nil {
		t.Fatalf("span = %v..%v", summary["first_touch"], summary["last_touch"])
	}
	sessions := data["sessions"].([]any)
	session := sessions[0].(map[string]any)["session"].(map[string]any)
	if session["first_touch"] != nil || session["last_touch"] != nil {
		t.Fatalf("session span = %v..%v", session["first_touch"], session["last_touch"])
	}
	if _, ok := sessions[0].(map[string]any)["open"].(map[string]any)["turn_id"]; ok {
		t.Fatal("null timestamps must not carry turn ids")
	}
}

func TestFaBuildDataScanCapFlagsTruncation(t *testing.T) {
	touches := make([]faTouch, 0, faScanCap+1)
	for i := 0; i < faScanCap+1; i++ {
		touches = append(touches, faTouchFixture(
			"sess-"+sprintf("%04d", i), "ev-"+sprintf("%04d", i), "Edit", "path_suffix", "/repos/main", int64(faScanCap+1-i)))
	}
	warnings := []string{}
	data := faBuildData(faDataArgs(50), &faTail{Rel: "crates/foo/tee.rs"}, touches, &warnings)
	summary := data["summary"].(map[string]any)
	if summary["total_touches"] != faScanCap || summary["scan_truncated"] != true || data["truncated"] != true {
		t.Fatalf("summary = %#v truncated = %v", summary, data["truncated"])
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "matched more than 2000 touches") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestFaSessionRollupsRankContiguousWhenMiddleRowSkipped(t *testing.T) {
	touches := []faTouch{
		faTouchFixture("sess-a", "ev-a", "Edit", "path_suffix", "/repos/main", 3000),
		faTouchFixture("", "ev-bad", "Edit", "path_suffix", "/repos/main", 2000),
		faTouchFixture("sess-b", "ev-b", "Edit", "path_suffix", "/repos/main", 1000),
	}
	sessions, _, _ := faSessionRollups(touches, 50)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	for i, want := range []string{"session:" + qB64URL([]byte("sess-a")), "session:" + qB64URL([]byte("sess-b"))} {
		item := sessions[i].(map[string]any)
		if item["rank"] != i+1 || item["id"] != want {
			t.Fatalf("sessions[%d] = %#v, want rank %d id %s", i, item, i+1, want)
		}
	}
}

func TestFaActionPreviewCollapsesWhitespaceAndTruncatesByChars(t *testing.T) {
	touch := faTouchFixture("sess-a", "ev-a", "Edit", "path_suffix", "/repos/main", 1000)
	touch.InputPreview = "  edit\n\t " + strings.Repeat("字", 400)
	preview := faActionPreview(&touch).(string)
	if utf8.RuneCountInString(preview) != 161 || !strings.HasSuffix(preview, "…") || !strings.HasPrefix(preview, "edit") {
		t.Fatalf("preview len = %d", utf8.RuneCountInString(preview))
	}
	touch.InputPreview = "  a\n  b\t "
	if got := faActionPreview(&touch).(string); got != "a b" {
		t.Fatalf("collapsed = %q", got)
	}
}

// --- format_text ---

func TestFaFormatTextDescribesUnknownOnlyRootsAsIncompleteAttribution(t *testing.T) {
	data := map[string]any{
		"tail":        "crates/foo/tee.rs",
		"scope":       "all",
		"granularity": "sessions",
		"summary": map[string]any{
			"total_touches":        2,
			"distinct_sessions":    2,
			"distinct_roots":       1,
			"distinct_known_roots": 0,
			"unknown_root_touches": 2,
			"ambiguous":            true,
		},
		"sessions": []any{},
	}
	text := faText(map[string]any{"data": data, "warnings": []string{}})
	if !strings.Contains(text, "Worktree-root attribution is incomplete") {
		t.Fatalf("missing incomplete line:\n%s", text)
	}
	if strings.Contains(text, "matched more than one worktree root") {
		t.Fatalf("unknown-only spread is not ambiguity:\n%s", text)
	}
}

func TestFaFormatTextReportsKnownRootSpreadAndUnknownAttributionSeparately(t *testing.T) {
	data := map[string]any{
		"tail":        "crates/foo/tee.rs",
		"scope":       "all",
		"granularity": "sessions",
		"summary": map[string]any{
			"total_touches":        3,
			"distinct_sessions":    3,
			"distinct_roots":       3,
			"distinct_known_roots": 2,
			"unknown_root_touches": 1,
			"ambiguous":            true,
		},
		"sessions": []any{},
	}
	text := faText(map[string]any{"data": data, "warnings": []string{}})
	if !strings.Contains(text, "matched more than one worktree root") {
		t.Fatalf("missing ambiguity line:\n%s", text)
	}
	if !strings.Contains(text, "Worktree-root attribution is incomplete") {
		t.Fatalf("missing incomplete line:\n%s", text)
	}
}

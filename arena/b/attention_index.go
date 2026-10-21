package main

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var faLiteralRun = regexp.MustCompile(`[A-Za-z0-9._-]+`)

func faTouches(_ context.Context, db *Store, q faQuery) ([]faTouch, error) {
	if q.Rel == "" {
		return nil, argErrorf("file_attention requires a non-empty path tail")
	}
	if q.ProjectScope && q.NormalizedPID == "" {
		return []faTouch{}, nil
	}
	rel := q.Rel
	relRe := regexp.QuoteMeta(rel)
	slashRe := regexp.QuoteMeta("/" + rel)
	keys := strings.Join(faPathKeys, "|")
	scalar := regexp.MustCompile(`"(?:` + keys + `)"[[:space:]]*:[[:space:]]*"((?:[^"\\]|\\.)*` + slashRe + `|` + relRe + `)"`)
	array := regexp.MustCompile(`"(?:` + keys + `)"[[:space:]]*:[[:space:]]*\[[^\]]*"((?:[^"\\]|\\.)*` + slashRe + `|` + relRe + `)"`)
	shell := regexp.MustCompile("(^|[[:space:]'\"`=(])(/[^/][^[:space:]'\"`<>|;&),]*" + slashRe + "|/" + relRe + "|" + relRe + ")([[:space:]'\"`,;|&<>)]|$)")
	provenance := regexp.MustCompile(`"(?:` + keys + `|command|cmd)"[[:space:]]*:`)
	runs := faLiteralRun.FindAllString(rel, -1)
	var exact, suffix []faTouch
	for _, e := range db.events {
		tool := strings.ToLower(e.ToolName)
		if e.ToolPhase != "request" || tool == "file_attention" || strings.HasSuffix(tool, "_file_attention") {
			continue
		}
		if q.Tool != "" && !strings.EqualFold(e.ToolName, q.Tool) || q.Harness != "" && e.Harness != q.Harness || q.SourceName != "" && e.SourceName != q.SourceName {
			continue
		}
		if q.MutationsOnly && strings.Contains(faReadToolNames, "'"+tool+"'") {
			continue
		}
		if q.StartUnixMs != nil && e.Time < *q.StartUnixMs || q.EndUnixMs != nil && e.Time >= *q.EndUnixMs {
			continue
		}
		makeTouch := func(path, kind, root string) faTouch {
			t := faTouch{SessionID: e.SessionID, EventUID: e.EventUID, ToolCallID: e.ToolCallID, Harness: e.Harness, SourceName: e.SourceName, ToolName: e.ToolName, ToolPhase: e.ToolPhase, MatchedPath: path, MatchKind: kind, WorktreeRoot: root, CWD: e.Cwd, EventUnixMs: &e.Time, EventOrder: e.Order, TurnSeq: &e.Turn}
			if e.IO != nil {
				t.InputPreview = e.IO.InputPreview
				t.OutputPreview = e.IO.OutputPreview
			}
			return t
		}
		if e.RepoRelPath == rel && e.ProjectID != "" && (!q.ProjectScope || e.ProjectID == q.NormalizedPID) {
			path, root := rel, ""
			if faIsSinglePathCandidate(e.WorktreeRoot) {
				root = e.WorktreeRoot
				path = root + "/" + rel
			}
			exact = append(exact, makeTouch(path, "path_suffix", root))
		}
		io := e.IO
		if io == nil {
			continue
		}
		contains := true
		for _, run := range runs {
			if !strings.Contains(io.Input, run) {
				contains = false
				break
			}
		}
		if !contains {
			continue
		}
		var obj map[string]json.RawMessage
		_ = json.Unmarshal([]byte(io.Input), &obj)
		jstr := func(k string) string {
			r := obj[k]
			if len(r) == 0 || string(r) == "null" {
				return ""
			}
			var s string
			if json.Unmarshal(r, &s) == nil {
				return s
			}
			return string(r)
		}
		var paths []string
		legacy := ""
		for _, k := range faPathKeys {
			v := jstr(k)
			if v != "" {
				paths = append(paths, v)
			}
			if legacy == "" && (v == rel || strings.HasSuffix(v, "/"+rel)) {
				legacy = v
			}
		}
		nested := ""
		if m := scalar.FindStringSubmatch(io.Input); len(m) > 1 {
			nested = m[1]
		} else if m := array.FindStringSubmatch(io.Input); len(m) > 1 {
			nested = m[1]
		}
		cmd := jstr("command")
		if cmd == "" {
			cmd = jstr("cmd")
		}
		if legacy == "" && nested == "" && !shell.MatchString(cmd) {
			continue
		}
		candidate := ""
		if q.DeriveLegacy && !strings.HasPrefix(rel, "/") && len(provenance.FindAllStringIndex(io.Input, -1)) == 1 && len(paths) == 1 && faIsSinglePathCandidate(legacy) && faIsSinglePathCandidate(e.Cwd) && (legacy == rel || legacy == e.Cwd+"/"+rel) {
			candidate = e.Cwd
		}
		verified := ""
		if slices.Contains(q.NormalizedRoots, candidate) {
			verified = candidate
		}
		if q.ProjectScope {
			legacyTool := io.Project != "" && !strings.HasPrefix(io.Project, "git:") && faIsSinglePathCandidate(io.Root) && slices.Contains(q.NormalizedRoots, io.Root)
			legacyEvent := e.ProjectID != "" && !strings.HasPrefix(e.ProjectID, "git:") && faIsSinglePathCandidate(e.WorktreeRoot) && slices.Contains(q.NormalizedRoots, e.WorktreeRoot)
			if !(io.Project == q.NormalizedPID || legacyTool || io.Project == "" && e.ProjectID == q.NormalizedPID || io.Project == "" && candidate != "" && (legacyEvent || e.ProjectID == "" && verified != "")) {
				continue
			}
		}
		root := ""
		if io.Project != "" && io.Path == rel && faIsSinglePathCandidate(io.Root) {
			root = io.Root
		} else if candidate != "" {
			root = candidate
			if q.ProjectScope {
				root = verified
			}
		}
		path := firstNonEmpty(legacy, nested)
		kind := "path_suffix"
		if path == "" {
			kind = "shell_path"
		}
		suffix = append(suffix, makeTouch(path, kind, root))
	}
	limit := q.MaxRows + 1
	capTouches := func(ts []faTouch) []faTouch {
		sort.SliceStable(ts, func(i, j int) bool { return faTouchLess(&ts[i], &ts[j]) })
		if len(ts) > limit {
			ts = ts[:limit]
		}
		return ts
	}
	return faMergeTouches(append(capTouches(exact), capTouches(suffix)...), limit), nil
}

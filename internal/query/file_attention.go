package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

type fileAttention struct{}

func (fileAttention) Name() string { return "file_attention" }

func init() { register(fileAttention{}) }

const (
	faMinLimit        = 1
	faDefaultLimit    = 50
	faMinTailSegments = 2
	faScanCap         = 2000
)

func (fileAttention) Call(ctx context.Context, db *sql.DB, args json.RawMessage) (any, string, error) {
	a, err := faParseArgs(args)
	if err != nil {
		return nil, "", err
	}
	request := faCanonicalRequest(a)

	warnings := []string{}
	tail := faResolveTail(a.Path)
	if tail.Normalized {
		warnings = append(warnings, sprintf("normalized %q to %q before matching.", a.Path, tail.Rel))
	}
	if strings.HasPrefix(tail.Rel, "/") {
		warnings = append(warnings, sprintf(
			"could not reduce %q to a project-relative tail (no Git boundary or launch-directory containment could be proven); matching the absolute path literally, so other roots of the same file will not be unified. Pass a project-relative path for cross-root coverage.",
			a.Path))
	}
	if tail.Root == "" {
		warnings = append(warnings,
			"could not prove the path is a project-relative file in this launch scope; roots are derived only from exact relative captures with cwd, otherwise reported as unknown to avoid mislabeling arbitrary suffix matches.")
	}
	if depth := faTailSegments(tail.Rel); depth < faMinTailSegments {
		warnings = append(warnings, sprintf(
			"%q is a generic tail (depth %d); results may include unrelated files. Pass a longer project-relative path, and check the surfaced roots.",
			tail.Rel, depth))
	}
	if a.Scope == faScopeProject && tail.ProjID == "" {
		warnings = append(warnings,
			"scope=\"project\" could not establish the launch project's normalized identity; the project query remains closed rather than widening across projects.")
	}
	if a.Scope == faScopeProject && strings.HasPrefix(tail.ProjID, "git:") {
		warnings = append(warnings,
			"pre-digest worktree roots pruned before durable project mapping was installed cannot be attributed safely and remain excluded; currently registered roots are migrated and future normalized roots remain durable.")
	}

	q := faQuery{
		Rel:             tail.Rel,
		NormalizedPID:   tail.ProjID,
		NormalizedRoots: faRootsFor(tail.Root),
		DeriveLegacy:    tail.Root != "",
		ProjectScope:    a.Scope == faScopeProject,
		StartUnixMs:     a.StartUnixMs,
		EndUnixMs:       a.EndUnixMs,
		Tool:            a.Tool,
		Harness:         a.Harness,
		SourceName:      a.Source,
		MutationsOnly:   a.MutationsOnly,
		MaxRows:         faScanCap,
	}
	touches, err := faTouches(ctx, db, q)
	if err != nil {
		return nil, "", err
	}
	data := faBuildData(a, &tail, touches, &warnings)
	payload := map[string]any{
		"request":  request,
		"data":     data,
		"warnings": warnings,
	}
	return data, faText(payload), nil
}

func faRootsFor(root string) []string {
	if root == "" {
		return nil
	}
	return qWorktreeRootsForRepoRoot(root)
}

type faScope string

const (
	faScopeProject faScope = "project"
	faScopeAll     faScope = "all"
)

type faGranularity string

const (
	faGranularitySessions faGranularity = "sessions"
	faGranularityEvents   faGranularity = "events"
)

type faArgs struct {
	Path          string
	Scope         faScope
	Granularity   faGranularity
	StartDatetime *string
	EndDatetime   *string
	StartUnixMs   *int64
	EndUnixMs     *int64
	Tool          string
	Harness       string
	Source        string
	MutationsOnly bool
	Limit         int
	limitSet      bool
}

func faParseArgs(raw json.RawMessage) (*faArgs, error) {
	fields, err := lsObjectFields(raw,
		"path", "scope", "granularity", "start_datetime", "end_datetime",
		"mutations_only", "limit", "tool", "harness", "source")
	if err != nil {
		return nil, argErrorf("file_attention expects a JSON object with valid fields")
	}
	a := &faArgs{Scope: faScopeProject, Granularity: faGranularitySessions}
	getStr := func(key string) (*string, bool, error) {
		v, ok := fields[key]
		if !ok || string(v) == "null" {
			return nil, false, nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, true, err
		}
		return &s, true, nil
	}
	pathPtr, present, err := getStr("path")
	if err != nil {
		return nil, argErrorf("file_attention expects a JSON object with valid fields")
	}
	if !present || pathPtr == nil {
		return nil, argErrorf("path is required")
	}
	path := *pathPtr
	if path != strings.TrimSpace(path) {
		return nil, argErrorf("path must not have leading or trailing whitespace")
	}
	if strings.HasPrefix(path, "file://") {
		return nil, argErrorf("file:// URIs are not supported; pass a filesystem path")
	}
	if strings.HasSuffix(path, "/") {
		return nil, argErrorf("path must name a file, not a directory-style path ending in '/'")
	}
	if path == "" {
		return nil, argErrorf("path must be a non-empty string")
	}
	a.Path = path

	maxLimit := faMaxResults
	if v, ok := fields["limit"]; ok && string(v) != "null" {
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, argErrorf("file_attention expects a JSON object with valid fields")
		}
		lim, perr := n.Int64()
		if perr != nil {
			return nil, argErrorf("file_attention expects a JSON object with valid fields")
		}
		a.Limit = int(lim)
		a.limitSet = true
	}
	if !a.limitSet {
		a.Limit = faDefaultLimit
		if a.Limit > maxLimit {
			a.Limit = maxLimit
		}
	}
	if a.Limit < faMinLimit || a.Limit > maxLimit {
		return nil, argErrorf("limit must be between 1 and %d", maxLimit)
	}

	if v, ok := fields["start_datetime"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, argErrorf("file_attention expects a JSON object with valid fields")
		}
		ms, trimmed, aerr := qParseOptionalDatetime("start_datetime", s)
		if aerr != nil {
			return nil, aerr
		}
		if trimmed != "" {
			a.StartDatetime, a.StartUnixMs = &trimmed, &ms
		}
	}
	if v, ok := fields["end_datetime"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, argErrorf("file_attention expects a JSON object with valid fields")
		}
		ms, trimmed, aerr := qParseOptionalDatetime("end_datetime", s)
		if aerr != nil {
			return nil, aerr
		}
		if trimmed != "" {
			a.EndDatetime, a.EndUnixMs = &trimmed, &ms
		}
	}
	if a.StartUnixMs != nil && a.EndUnixMs != nil && *a.EndUnixMs <= *a.StartUnixMs {
		return nil, argErrorf("end_datetime must be later than start_datetime")
	}

	enum := func(key string, allowed map[string]bool) *string {
		v, ok := fields[key]
		if !ok || string(v) == "null" {
			return nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || !allowed[s] {
			return nil
		}
		return &s
	}
	badEnum := false
	if s := enum("scope", map[string]bool{string(faScopeProject): true, string(faScopeAll): true}); s != nil {
		a.Scope = faScope(*s)
	} else if v, ok := fields["scope"]; ok && string(v) != "null" {
		badEnum = true
	}
	if s := enum("granularity", map[string]bool{string(faGranularitySessions): true, string(faGranularityEvents): true}); s != nil {
		a.Granularity = faGranularity(*s)
	} else if v, ok := fields["granularity"]; ok && string(v) != "null" {
		badEnum = true
	}
	if badEnum {
		return nil, argErrorf("file_attention expects a JSON object with valid fields")
	}
	if v, ok := fields["mutations_only"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &a.MutationsOnly); err != nil {
			return nil, argErrorf("file_attention expects a JSON object with valid fields")
		}
	}
	if v, present, err := getStr("tool"); err != nil {
		return nil, argErrorf("file_attention expects a JSON object with valid fields")
	} else if present && v != nil {
		a.Tool = strings.TrimSpace(*v)
	}
	if a.Harness, err = faFilterField(fields, "harness"); err != nil {
		return nil, err
	}
	if a.Source, err = faFilterField(fields, "source"); err != nil {
		return nil, err
	}
	return a, nil
}

// faMaxResults is the MCP layer's max_results pin for this tool (tools.json).
const faMaxResults = 25

func faFilterField(fields map[string]json.RawMessage, field string) (string, error) {
	v, ok := fields[field]
	if !ok || string(v) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", argErrorf("file_attention expects a JSON object with valid fields")
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", argErrorf("%s must be a non-empty string when provided", field)
	}
	return trimmed, nil
}

func faCanonicalRequest(a *faArgs) map[string]any {
	req := map[string]any{
		"path":           a.Path,
		"scope":          string(a.Scope),
		"granularity":    string(a.Granularity),
		"tool":           strOrNil(a.Tool),
		"harness":        strOrNil(a.Harness),
		"source":         strOrNil(a.Source),
		"mutations_only": a.MutationsOnly,
		"limit":          a.Limit,
		"start_datetime": nil,
		"end_datetime":   nil,
	}
	if a.StartDatetime != nil {
		req["start_datetime"] = *a.StartDatetime
	}
	if a.EndDatetime != nil {
		req["end_datetime"] = *a.EndDatetime
	}
	return req
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

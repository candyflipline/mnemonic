package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// file_attention ports crates/moraine-mcp-core/src/file_attention_v1.rs over
// the DuckDB cache, with the touch SQL from clickhouse_repo/file_attention.rs.
// The mcp_event_locator/navigation tables have no cache counterpart, so the
// exact pass reads the canonical events columns directly and the suffix pass
// always scans payload tool IO (upstream's non-indexed legacy path).
type fileAttention struct{}

func (fileAttention) Name() string { return "file_attention" }

func init() { register(fileAttention{}) }

const (
	faMinLimit        = 1
	faDefaultLimit    = 50
	faMinTailSegments = 2
	// faScanCap bounds the matched rows pulled from the cache; the summary and
	// roots are computed over this set and hitting it flags truncation.
	faScanCap = 2000
)

func (fileAttention) Call(ctx context.Context, db *sql.DB, args json.RawMessage) (any, string, error) {
	a, err := faParseArgs(args)
	if err != nil {
		return nil, "", err
	}
	request := faCanonicalRequest(a)

	warnings := []string{}
	tail := faResolveTail(a.Path, "")
	if tail.Normalized {
		warnings = append(warnings, sprintf("normalized %q to %q before matching.", a.Path, tail.Rel))
	}
	if tail.TailIsAbsolute {
		warnings = append(warnings, sprintf(
			"could not reduce %q to a project-relative tail (no Git boundary or launch-directory containment could be proven); matching the absolute path literally, so other roots of the same file will not be unified. Pass a project-relative path for cross-root coverage.",
			a.Path))
	}
	if !tail.DeriveWorktreeRoots {
		warnings = append(warnings,
			"could not prove the path is a project-relative file in this launch scope; roots are derived only from exact relative captures with cwd, otherwise reported as unknown to avoid mislabeling arbitrary suffix matches.")
	}
	if depth := faTailSegments(tail.Rel); depth < faMinTailSegments {
		warnings = append(warnings, sprintf(
			"%q is a generic tail (depth %d); results may include unrelated files. Pass a longer project-relative path, and check the surfaced roots.",
			tail.Rel, depth))
	}
	if a.Scope == "project" && tail.ProjectID == "" {
		warnings = append(warnings,
			"scope=\"project\" could not establish the launch project's normalized identity; the project query remains closed rather than widening across projects.")
	}
	if a.Scope == "project" && strings.HasPrefix(tail.ProjectID, "git:") {
		warnings = append(warnings,
			"pre-digest worktree roots pruned before durable project mapping was installed cannot be attributed safely and remain excluded; currently registered roots are migrated and future normalized roots remain durable.")
	}
	// This server has no launch-directory scope, so a project-scoped query
	// never widens.
	if a.Scope == "all" {
		// Upstream warns when scope=all meets a project-scoped server; our
		// server is unscoped, so all is the ordinary widening path.
		_ = a
	}

	q := faQuery{
		Rel:                  tail.Rel,
		NormalizedProjectID:  tail.ProjectID,
		NormalizedProjectRoots: faRootsFor(tail.Root),
		DeriveLegacyRoots:    tail.DeriveWorktreeRoots,
		ApplyProjectScope:    a.Scope == "project",
		StartUnixMs:          a.StartUnixMs,
		EndUnixMs:            a.EndUnixMs,
		Tool:                 a.Tool,
		Harness:              a.Harness,
		SourceName:           a.Source,
		MutationsOnly:        a.MutationsOnly,
		MaxRows:              faScanCap,
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

// faRootsFor ports worktree_roots_for_repo_root for the resolved root.
func faRootsFor(root string) []string {
	if root == "" {
		return nil
	}
	return qWorktreeRootsForRepoRoot(root)
}

type faArgs struct {
	Path           string
	Scope          string // "project" | "all"
	Granularity    string // "sessions" | "events"
	StartDatetime  *string
	EndDatetime    *string
	StartUnixMs    *int64
	EndUnixMs      *int64
	Tool           string
	Harness        string
	Source         string
	MutationsOnly  bool
	Limit          int
}

// faParseArgs ports parse_file_attention_args + FileAttentionArgs::validate.
func faParseArgs(raw json.RawMessage) (*faArgs, error) {
	fields, err := lsObjectFields(raw)
	if err != nil {
		return nil, qErr("file_attention expects a JSON object with valid fields")
	}
	a := &faArgs{Scope: "project", Granularity: "sessions"}
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
		return nil, qErr("file_attention expects a JSON object with valid fields")
	}
	if !present || pathPtr == nil {
		return nil, qErrField("path", "path is required")
	}
	path := *pathPtr
	if path != strings.TrimSpace(path) {
		return nil, qErrField("path", "path must not have leading or trailing whitespace")
	}
	if strings.HasPrefix(path, "file://") {
		return nil, qErrField("path", "file:// URIs are not supported; pass a filesystem path")
	}
	if strings.HasSuffix(path, "/") {
		return nil, qErrField("path", "path must name a file, not a directory-style path ending in '/'")
	}
	if path == "" {
		return nil, qErrField("path", "path must be a non-empty string")
	}
	a.Path = path

	maxLimit := faMaxResults
	if v, ok := fields["limit"]; ok && string(v) != "null" {
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, qErr("file_attention expects a JSON object with valid fields")
		}
		lim, perr := n.Int64()
		if perr != nil {
			return nil, qErr("file_attention expects a JSON object with valid fields")
		}
		a.Limit = int(lim)
	}
	if a.Limit == 0 {
		a.Limit = faDefaultLimit
		if a.Limit > maxLimit {
			a.Limit = maxLimit
		}
	}
	if a.Limit < faMinLimit || a.Limit > maxLimit {
		return nil, qErrField("limit", sprintf("limit must be between 1 and %d", maxLimit))
	}

	if v, ok := fields["start_datetime"]; ok && string(v) != "null" {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, qErr("file_attention expects a JSON object with valid fields")
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
			return nil, qErr("file_attention expects a JSON object with valid fields")
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
		return nil, qErrField("end_datetime", "end_datetime must be later than start_datetime")
	}

	enum := func(key string, allowed map[string]bool) (string, error) {
		v, ok := fields[key]
		if !ok || string(v) == "null" {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", err
		}
		if !allowed[s] {
			return "", &json.UnsupportedValueError{}
		}
		return s, nil
	}
	var err2 error
	if a.Scope, err2 = enum("scope", map[string]bool{"project": true, "all": true}); err2 != nil {
		return nil, qErr("file_attention expects a JSON object with valid fields")
	}
	if a.Granularity, err2 = enum("granularity", map[string]bool{"sessions": true, "events": true}); err2 != nil {
		return nil, qErr("file_attention expects a JSON object with valid fields")
	}
	if v, ok := fields["mutations_only"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &a.MutationsOnly); err != nil {
			return nil, qErr("file_attention expects a JSON object with valid fields")
		}
	}
	if v, present, err := getStr("tool"); err != nil {
		return nil, qErr("file_attention expects a JSON object with valid fields")
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

func faMaxResults() int { return 25 }

// faFilterField ports normalize_optional_filter for an object field.
func faFilterField(fields map[string]json.RawMessage, field string) (string, error) {
	v, ok := fields[field]
	if !ok || string(v) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", qErr("file_attention expects a JSON object with valid fields")
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", qErrField(field, field+" must be a non-empty string when provided")
	}
	return trimmed, nil
}

// qErrField builds an ArgError the way invalid_request_with_field names the
// field: the field leads the message so the caller can route on it.
func qErrField(field, msg string) *ArgError {
	return qErr("%s: %s", field, msg)
}

func faCanonicalRequest(a *faArgs) map[string]any {
	req := map[string]any{
		"path":         a.Path,
		"scope":        a.Scope,
		"granularity":  a.Granularity,
		"tool":         strOrNil(a.Tool),
		"harness":      strOrNil(a.Harness),
		"source":       strOrNil(a.Source),
		"mutations_only": a.MutationsOnly,
		"limit":        a.Limit,
		"start_datetime": nil,
		"end_datetime": nil,
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

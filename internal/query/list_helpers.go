package query

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Helpers shared by the three tools this lane owns (list_sessions,
// file_attention, get_ingest_status). Names are q-prefixed so they can never
// collide with the helpers lane query-search adds to common.go.

// qErr builds an *ArgError with upstream's message text.
func qErr(format string, args ...any) *ArgError {
	return &ArgError{Msg: fmt.Sprintf(format, args...)}
}

// sortSliceStable is sort.SliceStable over any element type. search_repo.go
// (lane cache) calls it but no shared home exists yet; if common.go grows its
// own, delete this one.
func sortSliceStable[T any](xs []T, less func(a, b T) bool) {
	sort.SliceStable(xs, func(i, j int) bool { return less(xs[i], xs[j]) })
}

// qQuote renders a Go string as a single-quoted SQL literal. Filter values
// reach this only after parse validation; every events read still goes
// through here so quoting lives in one place.
func qQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// qOptQuote renders an optional string filter: NULL when absent.
func qOptQuote(s *string) string {
	if s == nil {
		return "NULL"
	}
	return qQuote(*s)
}

// qFormatMillis ports contract.rs format_rfc3339_utc_millis.
func qFormatMillis(unixMs int64) string {
	return time.UnixMilli(unixMs).UTC().Format("2006-01-02T15:04:05.000Z")
}

// qFormatDateTime renders a cached TIMESTAMP the way ClickHouse
// toString(DateTime64(3)) does; the display label and prose tests pin it.
func qFormatDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}

// qParseOptionalDatetime ports parse_optional_datetime: absent or blank is
// None; present input needs millisecond-bounded precision and an explicit
// timezone. Returns (unixMs, trimmed, nil) or an ArgError.
func qParseOptionalDatetime(field, input string) (int64, string, *ArgError) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return 0, "", nil
	}
	if err := qValidateMillisecondPrecision(field, trimmed); err != nil {
		return 0, "", err
	}
	ms, err := qParseExplicitTimezone(field, trimmed)
	if err != nil {
		return 0, "", err
	}
	return ms, trimmed, nil
}

// qValidateMillisecondPrecision ports validate_millisecond_precision.
func qValidateMillisecondPrecision(field, input string) *ArgError {
	timeStart := strings.IndexByte(input, 'T')
	if timeStart < 0 {
		timeStart = strings.IndexByte(input, 't')
		if timeStart < 0 {
			return nil
		}
	}
	timePart := input[timeStart+1:]
	dot := strings.IndexByte(timePart, '.')
	if dot < 0 {
		return nil
	}
	fraction := 0
	for _, ch := range timePart[dot+1:] {
		if ch < '0' || ch > '9' {
			break
		}
		fraction++
	}
	if fraction > 3 {
		return qErr("%s supports millisecond precision at most", field)
	}
	return nil
}

// qParseExplicitTimezone ports parse_explicit_timezone_datetime, returning
// unix milliseconds. The wrapped parse error text is Go's, not chrono's.
func qParseExplicitTimezone(field, input string) (int64, *ArgError) {
	if input == "" {
		return 0, qErr("%s must be a non-empty RFC 3339 datetime", field)
	}
	if !qHasExplicitTimezone(input) {
		return 0, qErr("%s must include an explicit timezone offset or Z", field)
	}
	t, err := time.Parse(time.RFC3339, input)
	if err != nil {
		return 0, qErr("%s must be a valid RFC 3339 datetime: %v", field, err)
	}
	return t.UnixMilli(), nil
}

// qHasExplicitTimezone ports has_explicit_timezone.
func qHasExplicitTimezone(input string) bool {
	input = strings.TrimSpace(input)
	if strings.HasSuffix(input, "Z") || strings.HasSuffix(input, "z") {
		return true
	}
	if len(input) < 6 {
		return false
	}
	o := input[len(input)-6:]
	return (o[0] == '+' || o[0] == '-') && isDigits(o[1:3]) && o[3] == ':' && isDigits(o[4:6])
}

func isDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// qNonEmptyFilter ports normalize_optional_filter.
func qNonEmptyFilter(field, raw string, present bool) (*string, *ArgError) {
	if !present {
		return nil, nil
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, qErr("%s must be a non-empty string when provided", field)
	}
	return &trimmed, nil
}

// qB64URL encodes base64url without padding (contract.rs encode_base64_url_no_pad).
func qB64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// qDecodeB64URL ports decode_base64_url_no_pad, including its
// padding-rejected strictness.
func qDecodeB64URL(s string) ([]byte, error) {
	if s == "" || len(s)%4 == 1 || strings.Contains(s, "=") {
		return nil, errors.New("bad base64")
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// qTypedID renders "session:<b64>" / "event:<b64>" handles.
func qTypedID(prefix, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", qErr("%s_id must be a non-empty string", strings.TrimSuffix(prefix, ":"))
	}
	return prefix + qB64URL([]byte(raw)), nil
}

// qTurnID renders "turn:<b64 session>:<seq>" (contract.rs McpTurnId).
func qTurnID(sessionID string, turnSeq uint32) string {
	return "turn:" + qB64URL([]byte(sessionID)) + ":" + fmt.Sprint(turnSeq)
}

// qInternalToolNames ports moraine-clickhouse mcp_tool_names::INTERNAL_TOOL_NAMES.
var qInternalToolNames = []string{"search", "search_sessions", "open", "list_sessions", "file_attention"}

// qInternalToolPredicate ports mcp_tool_names::sql_predicate for a trusted
// column expression: bare internal names and mcp__moraine__<leaf> aliases.
func qInternalToolPredicate(column string) string {
	norm := fmt.Sprintf("lower(trim(%s))", column)
	parts := fmt.Sprintf("string_split(%s, '__')", norm)
	names := make([]string, 0, len(qInternalToolNames))
	for _, n := range qInternalToolNames {
		names = append(names, qQuote(n))
	}
	leafIn := strings.Join(names, ", ")
	return fmt.Sprintf("(%s IN (%s) OR (len(%s) = 3 AND %s[1] = 'mcp' AND %s[2] = 'moraine' AND %s[3] IN (%s)))",
		norm, leafIn, parts, parts, parts, parts, leafIn)
}

// qProjectIDForRoot ports moraine-config project_id_for_repo_root: git
// repositories identify by their common dir, other directories by their
// canonical path.
func qProjectIDForRoot(root string) string {
	if common, ok := qGitCommonDir(root); ok {
		return qProjectIDForPath("moraine-git-common-dir\x00", "git", common)
	}
	canonical, ok := qCanonical(root)
	if !ok || !qIsDir(canonical) {
		return ""
	}
	return qProjectIDForPath("moraine-directory-root\x00", "dir", canonical)
}

func qProjectIDForPath(domain, prefix, path string) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte(path))
	return prefix + ":" + hex.EncodeToString(h.Sum(nil))
}

// qGitCommonDir ports moraine-config git_common_dir.
func qGitCommonDir(root string) (string, bool) {
	marker := root + "/.git"
	gitDir := marker
	if !qIsDir(marker) {
		b, err := os.ReadFile(marker)
		if err != nil {
			return "", false
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return "", false
		}
		path := strings.TrimSpace(rest)
		if path == "" {
			return "", false
		}
		if strings.HasPrefix(path, "/") {
			gitDir = path
		} else {
			gitDir = root + "/" + path
		}
	}
	common := gitDir
	if b, err := os.ReadFile(gitDir + "/commondir"); err == nil {
		cb := strings.TrimSpace(string(b))
		if strings.HasPrefix(cb, "/") {
			common = cb
		} else {
			common = gitDir + "/" + cb
		}
	}
	return qCanonical(common)
}

// qCanonical resolves symlinks like Rust's Path::canonicalize.
func qCanonical(path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", false
	}
	return abs, true
}

func qIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// qFindProjectRoot ports file_attention_v1.rs find_project_root: walk up from
// the path's parent looking for .git, bounded at $HOME or the filesystem root.
func qFindProjectRoot(path string) string {
	home := os.Getenv("HOME")
	dir := filepath.Dir(path)
	for {
		if qExists(dir + "/.git") {
			return dir
		}
		if home != "" && home == dir {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func qExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

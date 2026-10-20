package shared

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// UnparseableEventTS is the UTC fallback event_ts when a record carries no
// parseable timestamp.
const UnparseableEventTS = "1970-01-01 00:00:00.000"

var (
	sessionIDRe     = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)
	sessionDateRe   = regexp.MustCompile(`/(?:sessions|projects)/(\d{4})/(\d{2})/(\d{2})/`)
	rolloutTSRe     = regexp.MustCompile(`(?:^|/)rollout-(\d{4})-(\d{2})-(\d{2})T(\d{2})-(\d{2})-(\d{2})-`)
	naiveRecordTSRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?$`)
)

// InferSessionIDFromFile pulls the trailing UUID from a source file's stem.
func InferSessionIDFromFile(sourceFile string) string {
	stem := fileStem(sourceFile)
	if m := sessionIDRe.FindStringSubmatch(stem); m != nil {
		return m[1]
	}
	return ""
}

func fileStem(sourceFile string) string {
	name := sourceFile
	if i := strings.LastIndexByte(sourceFile, '/'); i >= 0 {
		name = sourceFile[i+1:]
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

// InferSessionDateFromFile prefers a /sessions|projects/YYYY/MM/DD/ path
// segment and otherwise falls back to the record timestamp's UTC date.
func InferSessionDateFromFile(sourceFile, recordTS string) string {
	if m := sessionDateRe.FindStringSubmatch(sourceFile); m != nil {
		return m[1] + "-" + m[2] + "-" + m[3]
	}
	if dt, ok := ParseRecordTS(recordTS); ok {
		return dt.UTC().Format("2006-01-02")
	}
	return "1970-01-01"
}

// InferRolloutRecordTSFromFile reads the rollout-YYYY-MM-DDTHH-MM-SS- file
// name convention; codex-only.
func InferRolloutRecordTSFromFile(sourceFile string) string {
	m := rolloutTSRe.FindStringSubmatch(sourceFile)
	if m == nil {
		return ""
	}
	return fmt.Sprintf("%s-%s-%sT%s:%s:%s.000Z", m[1], m[2], m[3], m[4], m[5], m[6])
}

// ParseRecordTS accepts RFC 3339 timestamps and naive
// YYYY-MM-DDTHH:MM:SS[.frac] forms (assumed UTC), like parse_record_ts.
func ParseRecordTS(recordTS string) (time.Time, bool) {
	trimmed := strings.TrimSpace(recordTS)
	if trimmed == "" {
		return time.Time{}, false
	}
	if dt, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return dt, true
	}
	if naiveRecordTSRe.MatchString(trimmed) {
		if dt, err := time.Parse("2006-01-02T15:04:05.999999999", trimmed); err == nil {
			return dt, true
		}
	}
	return time.Time{}, false
}

// FormatEventTS renders the DateTime64(3) spelling of a timestamp.
func FormatEventTS(dt time.Time) string {
	return dt.UTC().Format("2006-01-02 15:04:05.000")
}

// ParseEventTS mirrors parse_event_ts: the formatted timestamp plus whether
// the fallback had to be used.
func ParseEventTS(recordTS string) (string, bool) {
	if dt, ok := ParseRecordTS(recordTS); ok {
		return FormatEventTS(dt), false
	}
	return UnparseableEventTS, true
}

// FormatRecordTSMillis renders a timestamp as RFC 3339 with milliseconds and
// Z, the spelling claude-style sources carry in their records.
func FormatRecordTSMillis(dt time.Time) string {
	return dt.UTC().Format("2006-01-02T15:04:05.000Z")
}

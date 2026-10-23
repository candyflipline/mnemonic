package trace

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

func qOptQuote(s *string) string {
	if s == nil {
		return "NULL"
	}
	return sqlQuote(*s)
}

// qFormatDateTime renders a cached TIMESTAMP the way ClickHouse
// toString(DateTime64(3)) does; the display label and prose tests pin it.
func qFormatDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000")
}

func qParseOptionalDatetime(field, input string) (int64, string, *argError) {
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

func qValidateMillisecondPrecision(field, input string) *argError {
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
		return argErrorf("%s supports millisecond precision at most", field)
	}
	return nil
}

// qParseExplicitTimezone: the wrapped parse error text is Go's, not chrono's.
func qParseExplicitTimezone(field, input string) (int64, *argError) {
	if input == "" {
		return 0, argErrorf("%s must be a non-empty RFC 3339 datetime", field)
	}
	if !qHasExplicitTimezone(input) {
		return 0, argErrorf("%s must include an explicit timezone offset or Z", field)
	}
	t, err := time.Parse(time.RFC3339, input)
	if err != nil {
		return 0, argErrorf("%s must be a valid RFC 3339 datetime: %v", field, err)
	}
	return t.UnixMilli(), nil
}

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

func qNonEmptyFilter(field, raw string, present bool) (*string, *argError) {
	if !present {
		return nil, nil
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, argErrorf("%s must be a non-empty string when provided", field)
	}
	return &trimmed, nil
}

// qDecodeB64URL keeps upstream's padding-rejected strictness.
func qDecodeB64URL(s string) ([]byte, error) {
	if s == "" || len(s)%4 == 1 || strings.Contains(s, "=") {
		return nil, errors.New("bad base64")
	}
	return base64.RawURLEncoding.DecodeString(s)
}

func qTypedID(prefix, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", argErrorf("%s_id must be a non-empty string", strings.TrimSuffix(prefix, ":"))
	}
	return prefix + b64RawURL.EncodeToString([]byte(raw)), nil
}

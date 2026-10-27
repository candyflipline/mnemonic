package redact

// oracleRedactor is the regex redactor as of main 59796bc, the oracle for TestFuzzAgainstOracle.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const oracleRedactedMarker = "[REDACTED:"

type oracleRule struct {
	id          string
	re          *regexp.Regexp
	keywords    [][]byte
	secretGroup int
	gate        entropyGate
	minLen      int
	priority    int
}

type oracleSpan struct {
	start, end int
	ruleID     string
	priority   int
}

type oracleKeyword struct {
	text  []byte
	rules []int
}

type oracleRedactor struct {
	rules      []oracleRule
	keywords   []oracleKeyword
	alwaysScan []int
}

func newOracle(extraPatterns []string) (*oracleRedactor, error) {
	r := &oracleRedactor{}
	for i := range builtinRules {
		spec := &builtinRules[i]
		re, err := regexp.Compile(spec.pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile builtin redaction rule %s: %w", spec.id, err)
		}
		r.rules = append(r.rules, oracleRule{
			id:          spec.id,
			re:          re,
			keywords:    oracleLowerKeywords(spec.keywords),
			secretGroup: spec.secretGroup,
			gate:        spec.gate,
			minLen:      spec.minLen,
			priority:    spec.priority,
		})
	}
	for i, p := range extraPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("failed to compile redaction.extra_patterns[%d] for extra-pattern-%d: %w", i, i+1, err)
		}
		r.rules = append(r.rules, oracleRule{
			id:       fmt.Sprintf("extra-pattern-%d", i+1),
			re:       re,
			minLen:   1,
			priority: len(builtinRules) + i,
		})
	}
	byPriority := make(map[int]string, len(r.rules))
	for _, ru := range r.rules {
		if other, dup := byPriority[ru.priority]; dup {
			panic(fmt.Sprintf("redact: rules %s and %s share priority %d", other, ru.id, ru.priority))
		}
		byPriority[ru.priority] = ru.id
	}
	r.buildOraclePrefilter()
	return r, nil
}

func (r *oracleRedactor) buildOraclePrefilter() {
	seen := make(map[string]int)
	for i := range r.rules {
		kws := r.rules[i].keywords
		if len(kws) == 0 {
			r.alwaysScan = append(r.alwaysScan, i)
			continue
		}
		for _, kw := range kws {
			j, ok := seen[string(kw)]
			if !ok {
				j = len(r.keywords)
				seen[string(kw)] = j
				r.keywords = append(r.keywords, oracleKeyword{text: kw})
			}
			r.keywords[j].rules = append(r.keywords[j].rules, i)
		}
	}
}

func (r *oracleRedactor) Line(line []byte) []byte {
	if !json.Valid(line) {
		out, _ := r.scrub(line)
		return out
	}
	return r.redactJSON(line)
}

func (r *oracleRedactor) redactJSON(line []byte) []byte {
	type literal struct{ q0, q1 int }
	var lits []literal
	inStr, start := false, 0
	for i := 0; i < len(line); {
		c := line[i]
		if !inStr {
			if c == '"' {
				inStr, start = true, i
			}
			i++
			continue
		}
		if c == '\\' {
			i += 2
			continue
		}
		if c == '"' {
			lits = append(lits, literal{start, i})
			inStr = false
		}
		i++
	}
	if len(lits) == 0 {
		return line
	}
	var out []byte
	last := 0
	for _, l := range lits {
		decoded, ok := oracleDecodeJSONString(line[l.q0+1 : l.q1])
		if !ok {
			continue
		}
		scrubbed, counts := r.scrub(decoded)
		if len(counts) == 0 {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(line)+64)
		}
		out = append(out, line[last:l.q0]...)
		out = oracleAppendJSONString(out, scrubbed)
		last = l.q1 + 1
	}
	if out == nil {
		return line
	}
	return append(out, line[last:]...)
}

func oracleDecodeJSONString(content []byte) ([]byte, bool) {
	if bytes.IndexByte(content, '\\') < 0 {
		return content, true
	}
	out := make([]byte, 0, len(content))
	for i := 0; i < len(content); {
		c := content[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= len(content) {
			return nil, false
		}
		switch content[i+1] {
		case '"':
			out = append(out, '"')
			i += 2
		case '\\':
			out = append(out, '\\')
			i += 2
		case '/':
			out = append(out, '/')
			i += 2
		case 'b':
			out = append(out, '\b')
			i += 2
		case 'f':
			out = append(out, '\f')
			i += 2
		case 'n':
			out = append(out, '\n')
			i += 2
		case 'r':
			out = append(out, '\r')
			i += 2
		case 't':
			out = append(out, '\t')
			i += 2
		case 'u':
			r, ok := oracleHex4(content, i+2)
			if !ok {
				return nil, false
			}
			i += 6
			if utf16.IsSurrogate(r) && i+6 <= len(content) &&
				content[i] == '\\' && content[i+1] == 'u' {
				if r2, ok := oracleHex4(content, i+2); ok {
					if dec := utf16.DecodeRune(r, r2); dec != unicode.ReplacementChar {
						r = dec
						i += 6
					}
				}
			}
			out = utf8.AppendRune(out, r)
		default:
			return nil, false
		}
	}
	return out, true
}

func oracleHex4(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(b[i:i+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

func oracleAppendJSONString(dst []byte, s []byte) []byte {
	const hexDigits = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			i += size
			start = i
			continue
		}
		if r == 0x2028 || r == 0x2029 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func (r *oracleRedactor) scrub(text []byte) ([]byte, map[string]uint64) {
	spans := r.nonOverlappingSpans(text)
	if len(spans) == 0 {
		return text, nil
	}
	out := make([]byte, 0, len(text)+16*len(spans))
	counts := make(map[string]uint64, len(spans))
	last := 0
	for _, s := range spans {
		out = append(out, text[last:s.start]...)
		out = append(out, oracleRedactedMarker...)
		out = append(out, s.ruleID...)
		out = append(out, ']')
		last = s.end
		counts[s.ruleID]++
	}
	return append(out, text[last:]...), counts
}

func (r *oracleRedactor) nonOverlappingSpans(text []byte) []oracleSpan {
	cands := r.candidates(text)
	live := false
	for _, c := range cands {
		if c {
			live = true
			break
		}
	}
	if !live {
		return nil
	}
	ph := oraclePlaceholderRanges(text)
	var spans []oracleSpan
	for i := range r.rules {
		if !cands[i] {
			continue
		}
		ru := &r.rules[i]
		for _, loc := range ru.re.FindAllSubmatchIndex(text, -1) {
			g0, g1 := loc[2*ru.secretGroup], loc[2*ru.secretGroup+1]
			if g0 < 0 || oracleOverlapsAny(ph, g0, g1) || !utf8.Valid(text[g0:g1]) {
				continue
			}
			if !r.shouldRedact(ru, text[g0:g1]) {
				continue
			}
			spans = append(spans, oracleSpan{start: g0, end: g1, ruleID: ru.id, priority: ru.priority})
		}
	}
	slices.SortFunc(spans, func(a, b oracleSpan) int {
		if c := cmp.Compare(a.start, b.start); c != 0 {
			return c
		}
		if c := cmp.Compare(b.end-b.start, a.end-a.start); c != 0 {
			return c
		}
		return cmp.Compare(a.priority, b.priority)
	})
	kept := spans[:0]
	until := 0
	for _, s := range spans {
		if s.start < until {
			continue
		}
		until = s.end
		kept = append(kept, s)
	}
	return kept
}

func (r *oracleRedactor) shouldRedact(ru *oracleRule, candidate []byte) bool {
	if len(candidate) < ru.minLen || bytes.Contains(candidate, []byte(oracleRedactedMarker)) {
		return false
	}
	if ru.gate == gateGenericCredential {
		return oracleGenericCandidateIsSecret(candidate)
	}
	return true
}

func (r *oracleRedactor) candidates(text []byte) []bool {
	c := make([]bool, len(r.rules))
	for _, i := range r.alwaysScan {
		c[i] = true
	}
	if len(r.keywords) == 0 {
		return c
	}
	lower := oracleAsciiLower(text)
	for _, kw := range r.keywords {
		if bytes.Contains(lower, kw.text) {
			for _, i := range kw.rules {
				c[i] = true
			}
		}
	}
	return c
}

func oraclePlaceholderRanges(text []byte) [][2]int {
	var ranges [][2]int
	open := []byte(oracleRedactedMarker)
	for start := 0; ; {
		i := bytes.Index(text[start:], open)
		if i < 0 {
			break
		}
		s := start + i
		j := bytes.IndexByte(text[s:], ']')
		if j < 0 {
			break
		}
		e := s + j + 1
		ranges = append(ranges, [2]int{s, e})
		start = e
	}
	return ranges
}

func oracleOverlapsAny(ranges [][2]int, start, end int) bool {
	for _, r := range ranges {
		if start < r[1] && end > r[0] {
			return true
		}
	}
	return false
}

func oracleGenericCandidateIsSecret(candidate []byte) bool {
	trimmed := bytes.Trim(candidate, "\"'`,;")
	if len(trimmed) < 20 || oracleIsStructuralFalsePositive(trimmed) {
		return false
	}
	entropy := oracleShannonEntropy(trimmed)
	switch {
	case oracleIsHex(trimmed):
		return entropy-oracleAllDigitHexPenalty(trimmed) > 3.0
	case oracleIsBase64ish(trimmed):
		return entropy > 4.5
	default:
		return entropy > 3.5
	}
}

func oracleShannonEntropy(v []byte) float64 {
	if len(v) == 0 {
		return 0
	}
	var counts [256]int
	for _, b := range v {
		counts[b]++
	}
	n := float64(len(v))
	sum := 0.0
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			sum -= p * math.Log2(p)
		}
	}
	return sum
}

func oracleIsHex(v []byte) bool {
	if len(v) < 20 {
		return false
	}
	for _, b := range v {
		if b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' {
			continue
		}
		return false
	}
	return true
}

func oracleIsBase64ish(v []byte) bool {
	if len(v) < 20 {
		return false
	}
	for _, b := range v {
		if b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
			b == '+' || b == '/' || b == '_' || b == '-' || b == '=' {
			continue
		}
		return false
	}
	return true
}

func oracleAllDigitHexPenalty(v []byte) float64 {
	for _, b := range v {
		if b < '0' || b > '9' {
			return 0
		}
	}
	return 1.2 / math.Log2(float64(len(v)))
}

var oracleStructuralStopwords = [][]byte{
	[]byte("example"), []byte("placeholder"), []byte("changeme"),
	[]byte("notasecret"), []byte("not-a-secret"), []byte("dummy"),
	[]byte("sample"), []byte("redacted"), []byte("password"),
	[]byte("secret"), []byte("token"),
}

func oracleIsStructuralFalsePositive(v []byte) bool {
	lower := oracleAsciiLower(v)
	for _, w := range oracleStructuralStopwords {
		if bytes.Contains(lower, w) {
			return true
		}
	}
	rs := bytes.Runes(lower)
	if len(rs) == 0 {
		return true
	}
	same := true
	for _, r := range rs[1:] {
		if r != rs[0] {
			same = false
			break
		}
	}
	if same {
		return true
	}
	alnum := make([]byte, 0, len(lower))
	for _, b := range lower {
		if b >= '0' && b <= '9' || b >= 'a' && b <= 'z' {
			alnum = append(alnum, b)
		}
	}
	switch string(alnum) {
	case "abcdefghijklmnopqrstuvwxyz", "zyxwvutsrqponmlkjihgfedcba",
		"01234567890123456789", "12345678901234567890":
		return true
	}
	return false
}

func oracleAsciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func oracleLowerKeywords(kws []string) [][]byte {
	out := make([][]byte, 0, len(kws))
	for _, kw := range kws {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		out = append(out, oracleAsciiLower([]byte(kw)))
	}
	return out
}

// Package redact removes secrets from raw lines on the client, before they
// leave the machine.
//
// Upstream scrubs parsed string fields on the server; here the client scrubs
// the whole jsonl line before upload, and the server re-parses it. Line
// therefore walks the JSON string literals of a valid line and scrubs each
// literal's decoded content, so a replacement never touches JSON structure
// and a line with no secret comes back byte-identical.
package redact

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

type entropyGate uint8

const (
	gateNone entropyGate = iota
	gateGenericCredential
)

type rule struct {
	id          string
	re          *regexp.Regexp
	keywords    [][]byte
	secretGroup int
	gate        entropyGate
	minLen      int
	priority    int
}

type redactionSpan struct {
	start, end int
	ruleID     string
	priority   int
}

// Redactor is safe for concurrent use.
type Redactor struct {
	rules      []rule
	keywords   []keyword
	alwaysScan []int
}

type keyword struct {
	text  []byte
	rules []int
}

const redactedMarker = "[REDACTED:"

// rustSpace matches what the Rust regex \s class matches (Unicode
// White_Space); Go's \s is ASCII-only. Only generic-api-key uses \s upstream.
const rustSpace = `[ \t\n\v\f\r\x{0085}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`

const genericPattern = `(?i)\b(?:api[_-]?key|access[_-]?token|auth(?:orization)?|bearer|client[_-]?secret|credential|passwd|password|private[_-]?key|secret|token)\b["']?` +
	rustSpace + `*[:=]` + rustSpace + `*["']?([0-9A-Za-z_./+=-]{20,})["']?`

type ruleSpec struct {
	priority    int
	id          string
	pattern     string
	keywords    []string
	secretGroup int
	gate        entropyGate
	minLen      int
}

func literal(priority int, id, pattern string, keywords ...string) ruleSpec {
	return ruleSpec{priority: priority, id: id, pattern: pattern, keywords: keywords, minLen: 1}
}

func generic(priority int, id, pattern string, keywords ...string) ruleSpec {
	return ruleSpec{priority: priority, id: id, pattern: pattern, keywords: keywords, secretGroup: 1, gate: gateGenericCredential, minLen: 20}
}

var builtinRules = []ruleSpec{
	literal(0, "aws-access-token", `\b((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16})\b`, "a3t", "akia", "asia", "abia", "acca"),
	literal(1, "github-token", `\bgh[pousr]_[0-9A-Za-z_]{36,255}\b`, "ghp_", "gho_", "ghu_", "ghs_", "ghr_"),
	literal(2, "github-fine-grained-token", `\bgithub_pat_[0-9A-Za-z_]{82,255}\b`, "github_pat_"),
	literal(3, "gitlab-token", `\b(?:glpat|glft|glcbt|glsoat|gldt)-[0-9A-Za-z_-]{20,}\b`, "glpat-", "glft-", "glcbt-", "glsoat-", "gldt-"),
	literal(4, "slack-token", `\bxox[pbarose]-[0-9A-Za-z-]{10,}\b`, "xoxp-", "xoxb-", "xoxa-", "xoxr-", "xoxo-", "xoxe-", "xoxs-"),
	literal(5, "slack-webhook", `https://hooks\.slack\.com/services/[0-9A-Za-z/_+-]{20,}`, "hooks.slack.com/services"),
	literal(6, "google-api-key", `\bAIza[0-9A-Za-z_-]{35}\b`, "aiza"),
	literal(7, "google-oauth-token", `\bya29\.[0-9A-Za-z_-]{20,}\b`, "ya29."),
	literal(8, "pem-private-key", `(?s)-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----.*?-----END (?:[A-Z0-9]+ )?PRIVATE KEY-----`, "private key"),
	literal(9, "openssh-private-key", `(?s)-----BEGIN OPENSSH PRIVATE KEY-----.*?-----END OPENSSH PRIVATE KEY-----`, "openssh private key"),
	literal(10, "jwt", `\beyJ[0-9A-Za-z_-]{5,}\.eyJ[0-9A-Za-z_-]{5,}\.[0-9A-Za-z_-]*\b`, "eyj"),
	literal(11, "stripe-token", `\b(?:sk|rk|pk)_(?:test|live)_[0-9A-Za-z]{10,}\b`, "sk_test_", "sk_live_", "rk_test_", "rk_live_", "pk_test_", "pk_live_"),
	literal(12, "stripe-webhook-secret", `\bwhsec_[0-9A-Za-z]{20,}\b`, "whsec_"),
	literal(13, "anthropic-api-key", `\bsk-ant-[0-9A-Za-z_-]{20,}\b`, "sk-ant-"),
	literal(14, "openai-api-key", `\bsk-(?:proj-)?[0-9A-Za-z_-]{20,}\b`, "sk-", "sk-proj-"),
	literal(15, "huggingface-token", `\bhf_[0-9A-Za-z]{30,}\b`, "hf_"),
	literal(16, "sendgrid-api-key", `\bSG\.[0-9A-Za-z_-]{20,}\.[0-9A-Za-z_-]{20,}\b`, "sg."),
	literal(17, "discord-webhook", `https://discord(?:app)?\.com/api/webhooks/[0-9]{6,}/[0-9A-Za-z_-]{20,}`, "discord.com/api/webhooks", "discordapp.com/api/webhooks"),
	literal(18, "npm-token", `\bnpm_[0-9A-Za-z]{30,}\b`, "npm_"),
	literal(19, "pypi-token", `\bpypi-[0-9A-Za-z_-]{30,}\b`, "pypi-"),
	literal(20, "digitalocean-token", `\bdop_v1_[0-9a-f]{64}\b`, "dop_v1_"),
	literal(21, "databricks-token", `\bdapi[0-9a-f]{32}\b`, "dapi"),
	literal(22, "dockerhub-token", `\bdckr_pat_[0-9A-Za-z_-]{20,}\b`, "dckr_pat_"),
	literal(23, "linear-api-key", `\blin_api_[0-9A-Za-z]{30,}\b`, "lin_api_"),
	literal(24, "notion-token", `\bsecret_[0-9A-Za-z]{30,}\b`, "secret_"),
	generic(25, "generic-api-key", genericPattern, "api", "access", "auth", "bearer", "client", "credential", "passwd", "password", "private", "secret", "token"),
}

// New compiles the built-in rules plus extra regex patterns from config.
// Extra patterns get the ids upstream would give them (extra-pattern-1, ...)
// and, having no keywords, are always scanned rather than prefiltered.
func New(extraPatterns []string) (*Redactor, error) {
	r := &Redactor{}
	for i := range builtinRules {
		spec := &builtinRules[i]
		re, err := regexp.Compile(spec.pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to compile builtin redaction rule %s: %w", spec.id, err)
		}
		r.rules = append(r.rules, rule{
			id:          spec.id,
			re:          re,
			keywords:    lowerKeywords(spec.keywords),
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
		r.rules = append(r.rules, rule{
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
	r.buildPrefilter()
	return r, nil
}

func (r *Redactor) buildPrefilter() {
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
				r.keywords = append(r.keywords, keyword{text: kw})
			}
			r.keywords[j].rules = append(r.keywords[j].rules, i)
		}
	}
}

// Line returns line with every secret replaced exactly as upstream does. A
// replacement never introduces '"', '\\' or a raw newline, so a JSON line
// stays valid; a line with no secret comes back byte-identical.
func (r *Redactor) Line(line []byte) []byte {
	if !json.Valid(line) {
		out, _ := r.scrub(line)
		return out
	}
	return r.redactJSON(line)
}

func (r *Redactor) redactJSON(line []byte) []byte {
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
		decoded, ok := decodeJSONString(line[l.q0+1 : l.q1])
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
		out = appendJSONString(out, scrubbed)
		last = l.q1 + 1
	}
	if out == nil {
		return line
	}
	return append(out, line[last:]...)
}

// decodeJSONString turns a literal's content bytes into the text they
// denote. Content without escapes is returned as-is (same slice). A
// malformed escape only happens on input json.Valid let through despite
// that; the caller leaves the literal untouched then.
func decodeJSONString(content []byte) ([]byte, bool) {
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
			r, ok := hex4(content, i+2)
			if !ok {
				return nil, false
			}
			i += 6
			if utf16.IsSurrogate(r) && i+6 <= len(content) &&
				content[i] == '\\' && content[i+1] == 'u' {
				if r2, ok := hex4(content, i+2); ok {
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

func hex4(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(b[i:i+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}

// appendJSONString encodes s as a JSON string the way encoding/json does
// with SetEscapeHTML(false): only what JSON requires gets escaped, so the
// server's parser reads back exactly what was scrubbed.
func appendJSONString(dst []byte, s []byte) []byte {
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
			dst = append(dst, `\ufffd`...)
			i += size
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
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

func (r *Redactor) scrub(text []byte) ([]byte, map[string]uint64) {
	spans := r.nonOverlappingSpans(text)
	if len(spans) == 0 {
		return text, nil
	}
	out := make([]byte, 0, len(text)+16*len(spans))
	counts := make(map[string]uint64, len(spans))
	last := 0
	for _, s := range spans {
		out = append(out, text[last:s.start]...)
		out = append(out, redactedMarker...)
		out = append(out, s.ruleID...)
		out = append(out, ']')
		last = s.end
		counts[s.ruleID]++
	}
	return append(out, text[last:]...), counts
}

func (r *Redactor) nonOverlappingSpans(text []byte) []redactionSpan {
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
	ph := placeholderRanges(text)
	var spans []redactionSpan
	for i := range r.rules {
		if !cands[i] {
			continue
		}
		ru := &r.rules[i]
		for _, loc := range ru.re.FindAllSubmatchIndex(text, -1) {
			g0, g1 := loc[2*ru.secretGroup], loc[2*ru.secretGroup+1]
			if g0 < 0 || overlapsAny(ph, g0, g1) || !utf8.Valid(text[g0:g1]) {
				continue
			}
			if !r.shouldRedact(ru, text[g0:g1]) {
				continue
			}
			spans = append(spans, redactionSpan{start: g0, end: g1, ruleID: ru.id, priority: ru.priority})
		}
	}
	slices.SortFunc(spans, func(a, b redactionSpan) int {
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

func (r *Redactor) shouldRedact(ru *rule, candidate []byte) bool {
	if len(candidate) < ru.minLen || bytes.Contains(candidate, []byte(redactedMarker)) {
		return false
	}
	if ru.gate == gateGenericCredential {
		return genericCandidateIsSecret(candidate)
	}
	return true
}

func (r *Redactor) candidates(text []byte) []bool {
	c := make([]bool, len(r.rules))
	for _, i := range r.alwaysScan {
		c[i] = true
	}
	if len(r.keywords) == 0 {
		return c
	}
	lower := asciiLower(text)
	for _, kw := range r.keywords {
		if bytes.Contains(lower, kw.text) {
			for _, i := range kw.rules {
				c[i] = true
			}
		}
	}
	return c
}

// placeholderRanges finds existing [REDACTED:...] spans, which later rules
// must not touch (this is what makes scrubbing idempotent). Mirrors
// upstream, including giving up at the first unclosed marker.
func placeholderRanges(text []byte) [][2]int {
	var ranges [][2]int
	open := []byte(redactedMarker)
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

func overlapsAny(ranges [][2]int, start, end int) bool {
	for _, r := range ranges {
		if start < r[1] && end > r[0] {
			return true
		}
	}
	return false
}

func genericCandidateIsSecret(candidate []byte) bool {
	trimmed := bytes.Trim(candidate, "\"'`,;")
	if len(trimmed) < 20 || isStructuralFalsePositive(trimmed) {
		return false
	}
	entropy := shannonEntropy(trimmed)
	switch {
	case isHex(trimmed):
		return entropy-allDigitHexPenalty(trimmed) > 3.0
	case isBase64ish(trimmed):
		return entropy > 4.5
	default:
		return entropy > 3.5
	}
}

func shannonEntropy(v []byte) float64 {
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

func isHex(v []byte) bool {
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

func isBase64ish(v []byte) bool {
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

func allDigitHexPenalty(v []byte) float64 {
	for _, b := range v {
		if b < '0' || b > '9' {
			return 0
		}
	}
	return 1.2 / math.Log2(float64(len(v)))
}

var structuralStopwords = [][]byte{
	[]byte("example"), []byte("placeholder"), []byte("changeme"),
	[]byte("notasecret"), []byte("not-a-secret"), []byte("dummy"),
	[]byte("sample"), []byte("redacted"), []byte("password"),
	[]byte("secret"), []byte("token"),
}

func isStructuralFalsePositive(v []byte) bool {
	lower := asciiLower(v)
	for _, w := range structuralStopwords {
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

// asciiLower lowercases ASCII bytes only, matching Rust's
// to_ascii_lowercase / Aho-Corasick ascii_case_insensitive.
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func lowerKeywords(kws []string) [][]byte {
	out := make([][]byte, 0, len(kws))
	for _, kw := range kws {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		out = append(out, asciiLower([]byte(kw)))
	}
	return out
}

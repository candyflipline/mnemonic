package redact

// TestFuzzAgainstOracle proves the live redactor byte-identical to the
// frozen 59796bc oracle: a seeded generator biased at every builtin rule
// emits bare strings and JSON lines, and every output and every candidate
// set must match. Default 20,000 strings per go test run; set
// REDACT_FUZZ_N for more (the feature's proof run is 1,000,000).

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Explicit seed list; recorded in the lane report. Each seed drives one
// deterministic PCG stream over an equal slice of the run count.
var fuzzSeeds = []uint64{1, 42, 1337, 90210, 314159, 2718281828, 1618033988, 5772156649}

const (
	fuzzDigits   = "0123456789"
	fuzzUpper    = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	fuzzLower    = "abcdefghijklmnopqrstuvwxyz"
	fuzzAlphaNum = fuzzDigits + fuzzUpper + fuzzLower
	fuzzB64URL   = fuzzAlphaNum + "-_"
	fuzzB64      = fuzzB64URL + "+/="
	fuzzGeneric  = fuzzAlphaNum + "_./+=-"
)

// One entry per builtin rule family: the prefilter keywords (canonical
// case), the secret-run charset, and interesting lengths around the
// pattern's thresholds (min-1/min/min+1, then mid and 255/256 bounds).
var fuzzRules = []fuzzRule{
	{[]string{"A3T", "AKIA", "ASIA", "ABIA", "ACCA"}, fuzzUpper + "234567", []int{15, 16, 17}},
	{[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "ghp"}, fuzzB64URL, []int{35, 36, 37, 254, 255, 256}},
	{[]string{"github_pat_"}, fuzzB64URL, []int{81, 82, 83, 255, 256}},
	{[]string{"glpat-", "glft-", "glcbt-", "glsoat-", "gldt-", "glpat"}, fuzzAlphaNum + "-_", []int{19, 20, 21, 64}},
	{[]string{"xoxp-", "xoxb-", "xoxa-", "xoxr-", "xoxo-", "xoxe-", "xoxs-", "xox"}, fuzzAlphaNum + "-", []int{9, 10, 11, 64}},
	{[]string{"https://hooks.slack.com/services/"}, fuzzAlphaNum + "/_+-", []int{19, 20, 21, 64, 255, 256}},
	{[]string{"AIza"}, fuzzAlphaNum + "-_", []int{34, 35, 36}},
	{[]string{"ya29."}, fuzzAlphaNum + "-_", []int{19, 20, 21}},
	{[]string{"sk_test_", "sk_live_", "rk_test_", "rk_live_", "pk_test_", "pk_live_", "sk_"}, fuzzAlphaNum, []int{9, 10, 11}},
	{[]string{"whsec_"}, fuzzAlphaNum, []int{19, 20, 21}},
	{[]string{"sk-ant-", "sk-ant"}, fuzzAlphaNum + "-_", []int{19, 20, 21}},
	{[]string{"sk-", "sk-proj-"}, fuzzAlphaNum + "-_", []int{19, 20, 21, 64}},
	{[]string{"hf_"}, fuzzAlphaNum, []int{29, 30, 31}},
	{[]string{"npm_"}, fuzzAlphaNum, []int{29, 30, 31}},
	{[]string{"pypi-"}, fuzzAlphaNum + "-_", []int{29, 30, 31}},
	{[]string{"dop_v1_"}, fuzzLower + fuzzDigits, []int{63, 64, 65}},
	{[]string{"dapi"}, fuzzLower + fuzzDigits, []int{31, 32, 33}},
	{[]string{"dckr_pat_"}, fuzzAlphaNum + "-_", []int{19, 20, 21}},
	{[]string{"lin_api_"}, fuzzAlphaNum, []int{29, 30, 31}},
	{[]string{"secret_"}, fuzzAlphaNum, []int{29, 30, 31}},
	{[]string{"api", "access", "auth", "authorization", "bearer", "client", "credential", "passwd", "password", "private", "privatekey", "secret", "token"}, fuzzGeneric, []int{18, 19, 20, 21, 64}},
}

type fuzzRule struct {
	keywords []string
	charset  string
	lens     []int
}

var fuzzGenericWords = []string{
	"api", "apikey", "api_key", "access", "accessToken", "auth", "authorization",
	"bearer", "client", "clientSecret", "credential", "passwd", "password",
	"private", "privateKey", "secret", "token", "tokens", "tokenize", "key",
	"session_token", "aws_secret_access_key", "SECRET", "Token", "PASSWORD",
}

var fuzzNoiseWords = []string{
	"example", "placeholder", "changeme", "notasecret", "not-a-secret",
	"dummy", "sample", "redacted", "value", "config", "header", "payload",
	"signature", "BEGIN", "END", "-----BEGIN", "-----END", "-----",
	"\"use_middle\"", "'single'", "`back`", "(paren)", "[bracket]",
	"0123456789abcdef", "aaaabbbbccccdddd", "AbCdEfGhIjKlMnOpQrStUvWxYz",
}

var fuzzUnicode = []string{"é", "ü", "ñ", "中", "日", "😀", "☃", "", " ", "", ""}

var fuzzNeighbours = []byte{'a', 'Z', '0', '9', '_', '-', '.', '"', '\'', ' '}

var fuzzSpace = []string{" ", "  ", "\t", "\n", "\v", "\f", "\r", "", " ", "　"}

// Values aimed at the generic-api-key entropy gate: repeats, digits-only,
// hex-only, alphabets, structural stopwords, pass and fail cases.
var fuzzGateValues = []string{
	"", "short", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	"1234567890123456789012345678901234", "99999999999999999999",
	"0123456789abcdef0123456789abcdef", "ABCDEF0123456789ABCDEF0123456789",
	"abcdefghijklmnopqrstuvwxyz12", "0123456789012345678901234567890",
	"Vb9pQx7Lm2Nf8Rs4Ty6Ua0Kc3Wd5Ez", "Kk8mNn2Pp9Qq4Rr7Tt6Uu1Ww3Xx5Yy",
	"passwordpasswordpasswordpassword", "token token token token token",
	"example_key_placeholder_value_xx", "not-a-secret-value-123456789",
	"changeme_changeme_changeme_1234", "secret_secret_secret_secret_12",
	"a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p+", "....", "____", "====++++----____",
	"x_php_has_a_totally_normal_value",
}

func TestFuzzAgainstOracle(t *testing.T) {
	n := 20000
	if s := os.Getenv("REDACT_FUZZ_N"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			t.Fatalf("bad REDACT_FUZZ_N %q", s)
		}
		n = v
	}
	live, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	orc, err := newOracle(nil)
	if err != nil {
		t.Fatalf("newOracle: %v", err)
	}
	liveX, err := New([]string{`acme_internal_[0-9A-Za-z]{32}`, `(?i)team_secret_[a-z0-9]{10,}`})
	if err != nil {
		t.Fatalf("New extras: %v", err)
	}
	orcX, err := newOracle([]string{`acme_internal_[0-9A-Za-z]{32}`, `(?i)team_secret_[a-z0-9]{10,}`})
	if err != nil {
		t.Fatalf("newOracle extras: %v", err)
	}
	for si, seed := range fuzzSeeds {
		count := n / len(fuzzSeeds)
		if si < n%len(fuzzSeeds) {
			count++
		}
		rng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
		for i := 0; i < count; i++ {
			in := []byte(fuzzString(rng))
			if got, want := live.candidates(in), orc.candidates(in); !slices.Equal(got, want) {
				t.Fatalf("candidates differ: seed %d (0x%x) index %d\ngot  %v\nwant %v\ninput %q",
					seed, seed, si*count+i, got, want, in)
			}
			if got, want := live.Line(in), orc.Line(in); !bytes.Equal(got, want) {
				min := fuzzShrink(live, orc, in)
				t.Fatalf("Line differs: seed %d (0x%x) index %d\ninput      %q\nminimal    %q\ngot  %q\nwant %q",
					seed, seed, si*count+i, in, min, got, want)
			}
			if i%2 == 0 {
				if got, want := liveX.Line(in), orcX.Line(in); !bytes.Equal(got, want) {
					t.Fatalf("Line(extras) differs: seed %d (0x%x) index %d\ninput %q\ngot  %q\nwant %q",
						seed, seed, si*count+i, in, got, want)
				}
			}
		}
	}
}

// fuzzShrink halves away prefix or suffix while the mismatch persists, so
// the failure prints a small reproducer next to the full input.
func fuzzShrink(live *Redactor, orc *oracleRedactor, in []byte) []byte {
	fails := func(c []byte) bool {
		return !slices.Equal(live.candidates(c), orc.candidates(c)) ||
			!bytes.Equal(live.Line(c), orc.Line(c))
	}
	cur := bytes.Clone(in)
	for len(cur) > 1 {
		half := len(cur) / 2
		switch {
		case fails(cur[:half]):
			cur = cur[:half]
		case fails(cur[half:]):
			cur = cur[half:]
		default:
			return cur
		}
	}
	return cur
}

func fuzzString(rng *rand.Rand) string {
	var sb strings.Builder
	if rng.IntN(1000) == 0 { // rare large input
		sb.WriteString(strings.Repeat(fuzzSegment(rng), 1+rng.IntN(50)))
	}
	segments := 1 + rng.IntN(4)
	for i := 0; i < segments; i++ {
		if i > 0 {
			sb.WriteString(fuzzSpace[rng.IntN(len(fuzzSpace))])
			if rng.IntN(4) == 0 {
				sb.WriteString(fuzzNoise(rng))
			}
		}
		sb.WriteString(fuzzSegment(rng))
	}
	s := sb.String()
	switch rng.IntN(100) {
	case 0, 1, 2, 3, 4: // deliberately broken JSON
		w := fuzzWrapJSON(rng, s)
		return w[:max(1, len(w)-rng.IntN(3))]
	case 5, 6, 7, 8, 9, 10, 11, 12, 13, 14: // nested: literal containing escaped JSON
		return `{"input":"` + fuzzJSONEscape(rng, fuzzJSONEscape(rng, s)) + `","n":1}`
	case 15, 16, 17: // key from the noise vocabulary, not "text"
		return `{"` + fuzzNoise(rng) + `":"` + fuzzJSONEscape(rng, s) + `"}`
	default:
		if rng.IntN(100) < 45 {
			return fuzzWrapJSON(rng, s)
		}
		return s
	}
}

func fuzzWrapJSON(rng *rand.Rand, s string) string {
	body := s
	if !utf8.ValidString(body) {
		body = strings.ToValidUTF8(body, "�")
	}
	switch rng.IntN(10) {
	case 0:
		return `{"a":{"b":"` + fuzzJSONEscape(rng, body) + `"}}`
	case 1:
		return `[` + fuzzJSONEscape(rng, body) + `,3,"x"]`
	default:
		return `{"text":"` + fuzzJSONEscape(rng, body) + `"}`
	}
}

func fuzzJSONEscape(rng *rand.Rand, s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		default:
			switch {
			case r < 0x20:
				sb.WriteString(fmt.Sprintf("\\u%04x", r))
			case r == 'é' && rng.IntN(2) == 0:
				sb.WriteString("\\u00e9")
			default:
				sb.WriteRune(r)
			}
		}
	}
	return sb.String()
}

func fuzzSegment(rng *rand.Rand) string {
	switch rng.IntN(100) {
	case 0, 1, 2, 3:
		return fuzzPEM(rng)
	case 4, 5, 6, 7, 8, 9:
		return fuzzJWT(rng)
	case 10, 11, 12:
		return fuzzMarker(rng)
	case 13, 14, 15, 16, 17:
		return fuzzGenericExpr(rng)
	case 18, 19:
		return fuzzHook(rng)
	case 20, 21:
		return fuzzNoise(rng)
	default:
		return fuzzRuleToken(rng)
	}
}

func fuzzRuleToken(rng *rand.Rand) string {
	fr := fuzzRules[rng.IntN(len(fuzzRules))]
	kw := fuzzCase(rng, fr.keywords[rng.IntN(len(fr.keywords))])
	n := fr.lens[rng.IntN(len(fr.lens))]
	var sb strings.Builder
	sb.WriteString(kw)
	sb.WriteString(fuzzRun(rng, fr.charset, n))
	if rng.IntN(3) == 0 {
		sb.WriteString(fuzzCase(rng, fuzzRun(rng, fr.charset, 1+rng.IntN(3))))
	}
	out := sb.String()
	if rng.IntN(2) == 0 {
		out = string(fuzzNeighbours[rng.IntN(len(fuzzNeighbours))]) + out
	}
	if rng.IntN(2) == 0 {
		out += string(fuzzNeighbours[rng.IntN(len(fuzzNeighbours))])
	}
	return out
}

func fuzzGenericExpr(rng *rand.Rand) string {
	word := fuzzCase(rng, fuzzGenericWords[rng.IntN(len(fuzzGenericWords))])
	var val string
	if rng.IntN(3) == 0 {
		val = fuzzGateValues[rng.IntN(len(fuzzGateValues))]
	} else {
		val = fuzzRun(rng, fuzzGeneric, []int{18, 19, 20, 21, 40, 64}[rng.IntN(6)])
		if rng.IntN(4) == 0 {
			val += fuzzCase(rng, fuzzGenericWords[rng.IntN(len(fuzzGenericWords))]) + fuzzRun(rng, fuzzGeneric, 20)
		}
	}
	sep := []string{":", "=", ":"}[rng.IntN(3)]
	open := ""
	close := ""
	if rng.IntN(2) == 0 {
		open = string('"')
		if rng.IntN(2) == 0 {
			open = "'"
		}
		if rng.IntN(4) > 0 {
			close = open
		}
	}
	var sb strings.Builder
	if rng.IntN(3) == 0 { // glue the keyword into a longer word: \b fails
		sb.WriteString(fuzzCase(rng, fuzzRun(rng, fuzzLower, 1+rng.IntN(4))))
	}
	sb.WriteString(word)
	sb.WriteString(fuzzSpace[rng.IntN(len(fuzzSpace))])
	sb.WriteString(sep)
	sb.WriteString(fuzzSpace[rng.IntN(len(fuzzSpace))])
	sb.WriteString(open)
	sb.WriteString(val)
	sb.WriteString(close)
	if rng.IntN(2) == 0 {
		sb.WriteString(fuzzSpace[rng.IntN(len(fuzzSpace))])
	}
	return sb.String()
}

func fuzzPEM(rng *rand.Rand) string {
	kinds := []string{"PRIVATE KEY", "OPENSSH PRIVATE KEY", "RSA PRIVATE KEY", "ENCRYPTED PRIVATE KEY"}
	kind := kinds[rng.IntN(len(kinds))]
	endKind := kind
	switch rng.IntN(10) {
	case 0:
		endKind = kinds[rng.IntN(len(kinds))] // maybe mismatched
	case 1:
		return "-----BEGIN " + kind + "-----" // no body, no END
	}
	nl := []string{"\n", "\n", "\n", "\r\n"}[rng.IntN(4)]
	var sb strings.Builder
	dashes := "-----"
	if rng.IntN(10) == 0 {
		dashes = "----"
	}
	sb.WriteString(dashes + "BEGIN " + kind + "-----")
	lines := 1 + rng.IntN(3)
	for i := 0; i < lines; i++ {
		sb.WriteString(nl)
		sb.WriteString(fuzzRun(rng, fuzzB64, 60+rng.IntN(17)))
	}
	switch rng.IntN(10) {
	case 0, 1, 2:
		// no END line
	default:
		sb.WriteString(nl + "-----END " + endKind + "-----")
	}
	s := sb.String()
	if rng.IntN(10) == 0 {
		s = fuzzCase(rng, s)
	}
	return s
}

func fuzzJWT(rng *rand.Rand) string {
	var sb strings.Builder
	switch rng.IntN(4) {
	case 0: // full three-segment shape
		sb.WriteString("eyJ" + fuzzRun(rng, fuzzB64URL, 5+rng.IntN(40)))
		sb.WriteString(".eyJ" + fuzzRun(rng, fuzzB64URL, 5+rng.IntN(40)))
		sb.WriteString("." + fuzzRun(rng, fuzzB64URL, rng.IntN(44)))
	case 1: // two segments
		sb.WriteString("eyJ" + fuzzRun(rng, fuzzB64URL, 5+rng.IntN(40)) + "." + fuzzRun(rng, fuzzB64URL, 5+rng.IntN(40)))
	case 2: // almost: one segment
		sb.WriteString("eyJ" + fuzzRun(rng, fuzzB64URL, 5+rng.IntN(40)) + "." + fuzzRun(rng, fuzzGeneric, 25))
	default: // prefix glued to a word: \b fails
		sb.WriteString(fuzzCase(rng, "xeyJ"+fuzzRun(rng, fuzzB64URL, 20)))
	}
	s := sb.String()
	if rng.IntN(4) == 0 {
		s = fuzzCase(rng, s)
	}
	return s
}

func fuzzHook(rng *rand.Rand) string {
	switch rng.IntN(3) {
	case 0:
		return "https://hooks.slack.com/services/" + fuzzRun(rng, fuzzAlphaNum+"/_+-", 20+rng.IntN(40))
	case 1:
		host := []string{"discord.com", "discordapp.com"}[rng.IntN(2)]
		return "https://" + host + "/api/webhooks/" + fuzzRun(rng, fuzzDigits, 6+rng.IntN(12)) + "/" + fuzzRun(rng, fuzzB64URL, 20+rng.IntN(40))
	default:
		return "SG." + fuzzRun(rng, fuzzAlphaNum+"-_", 20+rng.IntN(10)) + "." + fuzzRun(rng, fuzzAlphaNum+"-_", 20+rng.IntN(10))
	}
}

func fuzzMarker(rng *rand.Rand) string {
	id := builtinRules[rng.IntN(len(builtinRules))].id
	switch rng.IntN(6) {
	case 0:
		return "[REDACTED:" + id // unclosed: placeholderRanges gives up
	case 1:
		return "[REDACTED:[" + fuzzRun(rng, fuzzAlphaNum, 25) + "]"
	case 2:
		return "[REDACTED:" + id + "][REDACTED:" + builtinRules[rng.IntN(len(builtinRules))].id + "]"
	default:
		return "[REDACTED:" + id + "]"
	}
}

func fuzzNoise(rng *rand.Rand) string {
	var sb strings.Builder
	n := 1 + rng.IntN(3)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(fuzzSpace[rng.IntN(len(fuzzSpace))])
		}
		switch rng.IntN(4) {
		case 0:
			sb.WriteString(fuzzCase(rng, fuzzNoiseWords[rng.IntN(len(fuzzNoiseWords))]))
		case 1:
			sb.WriteString(fuzzCase(rng, fuzzUnicode[rng.IntN(len(fuzzUnicode))]))
		case 2:
			sb.WriteString(fuzzRun(rng, fuzzAlphaNum, 1+rng.IntN(12)))
		default:
			sb.WriteString(fuzzCase(rng, fuzzGenericWords[rng.IntN(len(fuzzGenericWords))]))
		}
	}
	return sb.String()
}

// fuzzCase flips the case of ~30% of ASCII letters: enough canonical forms
// to match, enough mangled ones to exercise the case-insensitive prefilter
// against case-sensitive patterns.
func fuzzCase(rng *rand.Rand, s string) string {
	b := []byte(s)
	for i := range b {
		switch {
		case b[i] >= 'a' && b[i] <= 'z':
			if rng.IntN(100) < 30 {
				b[i] -= 32
			}
		case b[i] >= 'A' && b[i] <= 'Z':
			if rng.IntN(100) < 30 {
				b[i] += 32
			}
		}
	}
	return string(b)
}

func fuzzRun(rng *rand.Rand, charset string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rng.IntN(len(charset))]
	}
	return string(b)
}

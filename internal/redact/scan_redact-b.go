package redact

// Hand-written scanning primitives for builtin rules 13-24: literal
// prefixes, greedy {n,} class runs and ASCII \b, with the exact
// leftmost-first, non-overlapping semantics of the rule regexes reported
// through FindAllSubmatchIndex (see matchers.go). No regex engine.
//
// Helpers carry the b prefix; the sibling matchers_*.go lanes pick their
// own, and the package must still build once the lanes merge.

import "bytes"

// bClass is the one-character class of a {n,} run in a builtin pattern.
type bClass uint8

const (
	bClsWordDash bClass = iota // [0-9A-Za-z_-]
	bClsAlnum                  // [0-9A-Za-z]
	bClsDigit                  // [0-9]
)

// bInClass reports whether c is in class k.
func bInClass(c byte, k bClass) bool {
	switch c {
	case '_', '-':
		return k == bClsWordDash
	default:
		return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
	}
}

// bIsWord reports whether c is an ASCII word character, the alphabet of \b
// in the builtin patterns.
func bIsWord(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// bStartB reports whether the ASCII \b before the word character at s
// holds; every b-prefixed pattern starts on a word character.
func bStartB(text []byte, s int) bool {
	return s == 0 || !bIsWord(text[s-1])
}

// bRunEnd returns the end of the maximal run of class k starting at i.
func bRunEnd(text []byte, i int, k bClass) int {
	e := i
	for e < len(text) && bInClass(text[e], k) {
		e++
	}
	return e
}

// bTailEnd returns where the regex cls{min,}\b starting its run at i ends:
// greedy, so the largest p in [i+min, runEnd] where \b holds, which is
// word-ness differing across p (end of text counts as non-word). -1 when
// no allowed end has a boundary, which fails the whole match attempt —
// backtracking into the literal prefix cannot help.
func bTailEnd(text []byte, i, min int, k bClass) int {
	e := bRunEnd(text, i, k)
	if e-i < min {
		return -1
	}
	for p := e; ; p-- {
		if bIsWord(text[p-1]) != (p < len(text) && bIsWord(text[p])) {
			return p
		}
		if p == i+min {
			return -1
		}
	}
}

// bTailRule is the matcher for \bpat cls{min,}\b: each occurrence of the
// literal pat at a start boundary takes a greedy bounded class run; a
// failed attempt resumes one byte on, a match resumes at its end.
func bTailRule(pat string, min int, k bClass) matcher {
	p := []byte(pat)
	return func(text []byte, out [][2]int) [][2]int {
		for i := 0; i < len(text); {
			s := bytes.Index(text[i:], p)
			if s < 0 {
				return out
			}
			s += i
			if bStartB(text, s) {
				if e := bTailEnd(text, s+len(p), min, k); e >= 0 {
					out = append(out, [2]int{s, e})
					i = e
					continue
				}
			}
			i = s + 1
		}
		return out
	}
}

// bHexRule is the matcher for \bpat[0-9a-f]{n}\b: literal prefix, an exact
// run of lowercase hex, and the end boundary. {n} is exact, so no
// backtracking: the attempt fails unless all n are hex and \b holds.
func bHexRule(pat string, n int) matcher {
	p := []byte(pat)
	return func(text []byte, out [][2]int) [][2]int {
		for i := 0; i < len(text); {
			s := bytes.Index(text[i:], p)
			if s < 0 {
				return out
			}
			s += i
			e := s + len(p) + n
			// The last hex is a word character, so \b holds iff what
			// follows is not a word character (end of text counts).
			if e <= len(text) && bStartB(text, s) && bLowerHex(text[s+len(p):e]) && (e == len(text) || !bIsWord(text[e])) {
				out = append(out, [2]int{s, e})
				i = e
				continue
			}
			i = s + 1
		}
		return out
	}
}

// bLowerHex reports whether v is all [0-9a-f].
func bLowerHex(v []byte) bool {
	for _, c := range v {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// bOpenAI is \bsk-(?:proj-)?[0-9A-Za-z_-]{20,}\b. Leftmost-first: the
// sk-proj- branch is tried before the bare sk- branch, whose run may then
// consume the proj- itself; a failed occurrence resumes one byte on.
var bOpenAI = func(text []byte, out [][2]int) [][2]int {
	const proj = "proj-"
	p := []byte("sk-")
	for i := 0; i < len(text); {
		s := bytes.Index(text[i:], p)
		if s < 0 {
			return out
		}
		s += i
		if bStartB(text, s) {
			e := -1
			if bytes.HasPrefix(text[s+3:], []byte(proj)) {
				e = bTailEnd(text, s+3+len(proj), 20, bClsWordDash)
			}
			if e < 0 {
				e = bTailEnd(text, s+len(p), 20, bClsWordDash)
			}
			if e >= 0 {
				out = append(out, [2]int{s, e})
				i = e
				continue
			}
		}
		i = s + 1
	}
	return out
}

// bSendGrid is \bSG\.[0-9A-Za-z_-]{20,}\.[0-9A-Za-z_-]{20,}\b. Greedy run
// one cannot backtrack past the '.' — no run character is '.' — so the
// maximal run must be followed by the dot, then the bounded run.
var bSendGrid = func(text []byte, out [][2]int) [][2]int {
	p := []byte("SG.")
	for i := 0; i < len(text); {
		s := bytes.Index(text[i:], p)
		if s < 0 {
			return out
		}
		s += i
		if bStartB(text, s) {
			e1 := bRunEnd(text, s+len(p), bClsWordDash)
			if e1-(s+len(p)) >= 20 && e1 < len(text) && text[e1] == '.' {
				if e := bTailEnd(text, e1+1, 20, bClsWordDash); e >= 0 {
					out = append(out, [2]int{s, e})
					i = e
					continue
				}
			}
		}
		i = s + 1
	}
	return out
}

// bDiscordHook is
// https://discord(?:app)?\.com/api/webhooks/[0-9]{6,}/[0-9A-Za-z_-]{20,}.
// No \b anywhere. The host alternation prefers the app form; a greedy
// digit run cannot backtrack onto the '/', so the maximal run must be
// followed by it; the final run is greedy with nothing after it.
var bDiscordHook = func(text []byte, out [][2]int) [][2]int {
	const app, mid = "app", ".com/api/webhooks/"
	p := []byte("https://discord")
	for i := 0; i < len(text); {
		s := bytes.Index(text[i:], p)
		if s < 0 {
			return out
		}
		s += i
		h := s + len(p)
		e, ok := 0, false
		if bytes.HasPrefix(text[h:], []byte(app)) {
			e, ok = bHookTail(text, h+len(app), mid)
		}
		if !ok {
			e, ok = bHookTail(text, h, mid)
		}
		if ok {
			out = append(out, [2]int{s, e})
			i = e
			continue
		}
		i = s + 1
	}
	return out
}

// bHookTail matches mid[0-9]{6,}/[0-9A-Za-z_-]{20,} from after the host
// and reports the match end.
func bHookTail(text []byte, h int, mid string) (int, bool) {
	if !bytes.HasPrefix(text[h:], []byte(mid)) {
		return 0, false
	}
	d := bRunEnd(text, h+len(mid), bClsDigit)
	if d-(h+len(mid)) < 6 || d >= len(text) || text[d] != '/' {
		return 0, false
	}
	e := bRunEnd(text, d+1, bClsWordDash)
	if e-(d+1) < 20 {
		return 0, false
	}
	return e, true
}

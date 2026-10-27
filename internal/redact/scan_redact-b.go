package redact

// Hand-written scanning primitives for builtin rules 13-24: literal
// prefixes, greedy {n,} class runs and ASCII \b, with the exact
// leftmost-first, non-overlapping semantics of the rule regexes reported
// through FindAllSubmatchIndex (see matchers.go). No regex engine.
//
// Helpers carry the b prefix; the sibling matchers_*.go lanes pick their
// own, and the package must still build once the lanes merge.

import "bytes"

type bClass uint8

const (
	bClsWordDash bClass = iota
	bClsAlnum
	bClsDigit
)

func bInClass(c byte, k bClass) bool {
	switch c {
	case '_', '-':
		return k == bClsWordDash
	}
	if k == bClsDigit {
		return '0' <= c && c <= '9'
	}
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func bIsWord(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func bStartB(text []byte, s int) bool {
	return s == 0 || !bIsWord(text[s-1])
}

func bRunEnd(text []byte, i int, k bClass) int {
	e := i
	for e < len(text) && bInClass(text[e], k) {
		e++
	}
	return e
}

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

func bLowerHex(v []byte) bool {
	for _, c := range v {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

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

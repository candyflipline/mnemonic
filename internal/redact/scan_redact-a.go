package redact

import "bytes"

func isWordByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

func wordBoundary(text []byte, e int) bool {
	before := e > 0 && isWordByte(text[e-1])
	after := e < len(text) && isWordByte(text[e])
	return before != after
}

func classWord(c byte) bool { return isWordByte(c) }

func classWordDash(c byte) bool {
	return isWordByte(c) || c == '-'
}

func classAlnumDash(c byte) bool {
	return classAlnum(c) || c == '-'
}

func classAlnum(c byte) bool {
	return '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z'
}

func classAWS(c byte) bool {
	return 'A' <= c && c <= 'Z' || '2' <= c && c <= '7'
}

func classUpperDigit(c byte) bool {
	return 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func classHook(c byte) bool {
	return classAlnum(c) || c == '_' || c == '/' || c == '+' || c == '-'
}

func litPrefix(lit string) prefixFn {
	b := []byte(lit)
	return func(text []byte, i int) int {
		if bytes.HasPrefix(text[i:], b) {
			return len(lit)
		}
		return 0
	}
}

func prefixAWS(text []byte, i int) int {
	if len(text)-i < 4 || text[i] != 'A' {
		return 0
	}
	switch text[i+1] {
	case '3':
		if text[i+2] == 'T' && classUpperDigit(text[i+3]) {
			return 4
		}
	case 'K', 'S', 'B':
		if text[i+2] == 'I' && text[i+3] == 'A' {
			return 4
		}
	case 'C':
		if text[i+2] == 'C' && text[i+3] == 'A' {
			return 4
		}
	}
	return 0
}

func prefixGitHub(text []byte, i int) int {
	if len(text)-i < 4 || text[i] != 'g' || text[i+1] != 'h' || text[i+3] != '_' {
		return 0
	}
	switch text[i+2] {
	case 'p', 'o', 'u', 's', 'r':
		return 4
	}
	return 0
}

func prefixGitlab(text []byte, i int) int {
	if len(text)-i < 4 || text[i] != 'g' || text[i+1] != 'l' {
		return 0
	}
	switch text[i+2] {
	case 'p':
		if len(text)-i >= 6 && text[i+3] == 'a' && text[i+4] == 't' && text[i+5] == '-' {
			return 6
		}
	case 'f':
		if len(text)-i >= 5 && text[i+3] == 't' && text[i+4] == '-' {
			return 5
		}
	case 'c':
		if len(text)-i >= 6 && text[i+3] == 'b' && text[i+4] == 't' && text[i+5] == '-' {
			return 6
		}
	case 's':
		if len(text)-i >= 7 && text[i+3] == 'o' && text[i+4] == 'a' && text[i+5] == 't' && text[i+6] == '-' {
			return 7
		}
	case 'd':
		if len(text)-i >= 5 && text[i+3] == 't' && text[i+4] == '-' {
			return 5
		}
	}
	return 0
}

func prefixSlack(text []byte, i int) int {
	if len(text)-i < 5 || text[i] != 'x' || text[i+1] != 'o' || text[i+2] != 'x' || text[i+4] != '-' {
		return 0
	}
	switch text[i+3] {
	case 'p', 'b', 'a', 'r', 'o', 's', 'e':
		return 5
	}
	return 0
}

func prefixStripe(text []byte, i int) int {
	if len(text)-i < 8 {
		return 0
	}
	switch text[i] {
	case 's', 'r', 'p':
	default:
		return 0
	}
	if text[i+1] != 'k' || text[i+2] != '_' {
		return 0
	}
	switch text[i+3] {
	case 't':
		if text[i+4] == 'e' && text[i+5] == 's' && text[i+6] == 't' && text[i+7] == '_' {
			return 8
		}
	case 'l':
		if text[i+4] == 'i' && text[i+5] == 'v' && text[i+6] == 'e' && text[i+7] == '_' {
			return 8
		}
	}
	return 0
}

type (
	classFn  func(byte) bool
	prefixFn func([]byte, int) int
)

func scanToken(text []byte, out [][2]int, r tokenRule) [][2]int {
next:
	for i := 0; i < len(text); {
		pl := r.head(text, i)
		if pl == 0 || r.boundaryBefore && !wordBoundary(text, i) {
			i++
			continue
		}
		t := i + pl
		run := 0
		for t+run < len(text) && (r.maxRun == 0 || run < r.maxRun) && r.class(text[t+run]) {
			run++
		}
		if run < r.minRun {
			i++
			continue
		}
		e := t + run
		for {
			if !r.boundaryAfter || wordBoundary(text, e) {
				out = append(out, [2]int{i, e})
				i = e
				continue next
			}
			if e == t+r.minRun {
				i++
				continue next
			}
			e--
		}
	}
	return out
}

func pemTail(text []byte, p int, tail []byte) int {
	run := 0
	for p+run < len(text) && classUpperDigit(text[p+run]) {
		run++
	}
	for k := run; k >= 1; k-- {
		if p+k < len(text) && text[p+k] == ' ' && bytes.HasPrefix(text[p+k+1:], tail) {
			return p + k + 1 + len(tail)
		}
	}
	if bytes.HasPrefix(text[p:], tail) {
		return p + len(tail)
	}
	return -1
}

// The lazy dot of (?s)begin.*?end is a scan for the next end carrying its
// tail. Those ends are collected once and the begins, which only move
// forward, share one cursor over them: rescanning per begin is quadratic on
// lines that repeat a begin over end-less ends.
func scanPEM(text []byte, out [][2]int, begin, end []byte, kind bool, tail []byte) [][2]int {
	var ends [][2]int
	collected := false
	for s, k := 0, 0; ; {
		i := bytes.Index(text[s:], begin)
		if i < 0 {
			return out
		}
		s += i
		b := s + len(begin)
		if kind {
			if b = pemTail(text, b, tail); b < 0 {
				s++
				continue
			}
		}
		if !collected {
			ends, collected = pemEnds(text, end, kind, tail), true
		}
		for k < len(ends) && ends[k][0] < b {
			k++
		}
		if k == len(ends) {
			return out
		}
		out = append(out, [2]int{s, ends[k][1]})
		s = ends[k][1]
	}
}

func pemEnds(text []byte, end []byte, kind bool, tail []byte) [][2]int {
	var ends [][2]int
	for q := 0; ; q++ {
		j := bytes.Index(text[q:], end)
		if j < 0 {
			return ends
		}
		q += j
		e := q + len(end)
		if kind {
			if e = pemTail(text, e, tail); e < 0 {
				continue
			}
		}
		ends = append(ends, [2]int{q, e})
	}
}

// A failed attempt resumes at p1, the first out-of-class byte, not one byte
// on: every eyJ before p1 shares this attempt's failing tail, and a long
// class run would otherwise cost each eyJ prefix the whole line.
func scanJWT(text []byte, out [][2]int) [][2]int {
next:
	for i := 0; i+3 <= len(text); {
		if text[i] != 'e' || text[i+1] != 'y' || text[i+2] != 'J' || !wordBoundary(text, i) {
			i++
			continue
		}
		t := i + 3
		p1 := classRunEnd(text, t, classWordDash)
		if p1-t < 5 || p1 == len(text) || text[p1] != '.' {
			i = p1
			continue
		}
		u := p1 + 1
		if u+3 > len(text) || text[u] != 'e' || text[u+1] != 'y' || text[u+2] != 'J' {
			i = p1
			continue
		}
		v0 := u + 3
		p2 := classRunEnd(text, v0, classWordDash)
		if p2-v0 < 5 || p2 == len(text) || text[p2] != '.' {
			i = p1
			continue
		}
		v := p2 + 1
		p3 := classRunEnd(text, v, classWordDash)
		for e := p3; e >= v; e-- {
			if wordBoundary(text, e) {
				out = append(out, [2]int{i, e})
				i = e
				continue next
			}
		}
		i = p1
	}
	return out
}

func classRunEnd(text []byte, i int, class classFn) int {
	for i < len(text) && class(text[i]) {
		i++
	}
	return i
}

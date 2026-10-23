package redact

// Hand-written matchers for builtin rules 0-12 (wired up in
// matchers_a.go). Each one returns exactly the spans its rule's regex
// reports via FindAllSubmatchIndex, per the matcher contract in
// matchers.go, and is proven byte-identical to the frozen 59796bc oracle
// by TestFuzzAgainstOracle.
//
// Every class in these rules is ASCII-only and every rule is
// case-sensitive (only 8 and 9 carry a flag, (?s), which only makes dot
// match newlines), so matching on raw bytes is exact: a non-ASCII rune is
// one or more bytes outside every class, rune counts equal byte counts in
// runs, and an ASCII literal can only occur at a rune boundary.

import "bytes"

// isWordByte reports whether c is an ASCII word character, the character
// class of the \b in the builtin patterns. Bytes of multi-byte runes are
// never word characters, so the byte-level check is exact.
func isWordByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// wordBoundary reports whether a \b holds at e in text.
func wordBoundary(text []byte, e int) bool {
	before := e > 0 && isWordByte(text[e-1])
	after := e < len(text) && isWordByte(text[e])
	return before != after
}

// Run classes, named for the character classes in the patterns.
func classWord(c byte) bool { return isWordByte(c) } // [0-9A-Za-z_]

func classWordDash(c byte) bool { // [0-9A-Za-z_-]
	return isWordByte(c) || c == '-'
}

func classAlnumDash(c byte) bool { // [0-9A-Za-z-]
	return classAlnum(c) || c == '-'
}

func classAlnum(c byte) bool { // [0-9A-Za-z]
	return '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z'
}

func classAWS(c byte) bool { // [A-Z2-7]
	return 'A' <= c && c <= 'Z' || '2' <= c && c <= '7'
}

func classUpperDigit(c byte) bool { // [A-Z0-9]
	return 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

func classHook(c byte) bool { // [0-9A-Za-z/_+-]
	return classAlnum(c) || c == '_' || c == '/' || c == '+' || c == '-'
}

// Prefix heads. A prefix consumes the rule's fixed head at i and returns
// its length, or 0. Alternations in the heads (rule 0's A3T/AKIA/...,
// rule 3's glpat/glft/..., rule 11's sk/rk/pk and test/live) differ in a
// byte the head branches do not share, so at most one branch can match at
// any position and the regex's leftmost-first branch order cannot matter.
func litPrefix(lit string) prefixFn {
	b := []byte(lit)
	return func(text []byte, i int) int {
		if bytes.HasPrefix(text[i:], b) {
			return len(lit)
		}
		return 0
	}
}

// m00 head: (?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)
func prefixAWS(text []byte, i int) int {
	if len(text)-i < 4 || text[i] != 'A' {
		return 0
	}
	switch text[i+1] {
	case '3': // A3T[A-Z0-9]
		if text[i+2] == 'T' && classUpperDigit(text[i+3]) {
			return 4
		}
	case 'K', 'S', 'B': // AKIA ASIA ABIA
		if text[i+2] == 'I' && text[i+3] == 'A' {
			return 4
		}
	case 'C': // ACCA
		if text[i+2] == 'C' && text[i+3] == 'A' {
			return 4
		}
	}
	return 0
}

// m01 head: gh[pousr]_
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

// m03 head: (?:glpat|glft|glcbt|glsoat|gldt)-
func prefixGitlab(text []byte, i int) int {
	if len(text)-i < 4 || text[i] != 'g' || text[i+1] != 'l' {
		return 0
	}
	switch text[i+2] {
	case 'p': // glpat-
		if len(text)-i >= 6 && text[i+3] == 'a' && text[i+4] == 't' && text[i+5] == '-' {
			return 6
		}
	case 'f': // glft-
		if len(text)-i >= 5 && text[i+3] == 't' && text[i+4] == '-' {
			return 5
		}
	case 'c': // glcbt-
		if len(text)-i >= 6 && text[i+3] == 'b' && text[i+4] == 't' && text[i+5] == '-' {
			return 6
		}
	case 's': // glsoat-
		if len(text)-i >= 7 && text[i+3] == 'o' && text[i+4] == 'a' && text[i+5] == 't' && text[i+6] == '-' {
			return 7
		}
	case 'd': // gldt-
		if len(text)-i >= 5 && text[i+3] == 't' && text[i+4] == '-' {
			return 5
		}
	}
	return 0
}

// m04 head: xox[pbarose]-
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

// m11 head: (?:sk|rk|pk)_(?:test|live)_
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
	case 't': // test_
		if text[i+4] == 'e' && text[i+5] == 's' && text[i+6] == 't' && text[i+7] == '_' {
			return 8
		}
	case 'l': // live_
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

// scanToken is the shared shape of rules 0-7 and 11-12: an optional
// leading \b, a prefix, a greedy run of minRun..maxRun class characters
// (maxRun 0 means unbounded), an optional trailing \b. A greedy run meets
// \b by giving back one character at a time, so the match ends at the
// largest run length whose end satisfies the boundary; a fixed {n}
// (maxRun == minRun) has nothing to give back and checks one length.
func scanToken(text []byte, out [][2]int, lead bool, prefix prefixFn, class classFn, minRun, maxRun int, trail bool) [][2]int {
next:
	for i := 0; i < len(text); {
		pl := prefix(text, i)
		if pl == 0 || lead && !wordBoundary(text, i) {
			i++
			continue
		}
		t := i + pl
		run := 0
		for t+run < len(text) && class(text[t+run]) {
			run++
		}
		if run < minRun {
			i++
			continue
		}
		e := t + run
		if maxRun > 0 && e > t+maxRun {
			e = t + maxRun
		}
		for {
			if !trail || wordBoundary(text, e) {
				out = append(out, [2]int{i, e})
				i = e
				continue next
			}
			if e == t+minRun {
				i++
				continue next
			}
			e--
		}
	}
	return out
}

// pemTail consumes the PEM patterns' optional kind word (?:[A-Z0-9]+ )? at
// p and returns the position after it, or -1. Greedy like the regex: the
// run gives back characters one at a time while the tail literal does not
// follow the space, then the group is skipped and the tail must follow
// directly.
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

// scanPEM is the shape of rules 8 and 9: (?s)begin .*? end, where the lazy
// dot matches every byte, so laziness is a scan for the next end
// occurrence. For rule 8 begin is "-----BEGIN " and end is "-----END ",
// each followed by the kind word and "PRIVATE KEY-----" via pemTail; for
// rule 9 begin and end are the full literals and there is no kind.
func scanPEM(text []byte, out [][2]int, begin, end []byte, kind bool, tail []byte) [][2]int {
	for s := 0; ; {
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
		matched := false
		for q := b; ; {
			j := bytes.Index(text[q:], end)
			if j < 0 {
				break
			}
			q += j
			e := q + len(end)
			if kind {
				if e = pemTail(text, e, tail); e < 0 {
					q++
					continue
				}
			}
			out = append(out, [2]int{s, e})
			s = e
			matched = true
			break
		}
		if !matched {
			s++
		}
	}
}

// scanJWT is rule 10: \beyJ run{5,} \. eyJ run{5,} \. run* \b. A dot is
// not in the run class, so the maximal run before each dot cannot give
// characters back and the dots are where greediness leaves them; only the
// final unbounded run retreats for its \b.
func scanJWT(text []byte, out [][2]int) [][2]int {
next:
	for i := 0; i+3 <= len(text); {
		if text[i] != 'e' || text[i+1] != 'y' || text[i+2] != 'J' || !wordBoundary(text, i) {
			i++
			continue
		}
		t := i + 3
		run1 := classRun(text, t, classWordDash)
		if run1 < 5 || t+run1 >= len(text) || text[t+run1] != '.' {
			i++
			continue
		}
		u := t + run1 + 1
		if u+3 > len(text) || text[u] != 'e' || text[u+1] != 'y' || text[u+2] != 'J' {
			i++
			continue
		}
		v0 := u + 3
		run2 := classRun(text, v0, classWordDash)
		if run2 < 5 || v0+run2 >= len(text) || text[v0+run2] != '.' {
			i++
			continue
		}
		v := v0 + run2 + 1
		run3 := classRun(text, v, classWordDash)
		for e := v + run3; e >= v; e-- {
			if wordBoundary(text, e) {
				out = append(out, [2]int{i, e})
				i = e
				continue next
			}
		}
		i++
	}
	return out
}

func classRun(text []byte, i int, class classFn) int {
	n := 0
	for i+n < len(text) && class(text[i+n]) {
		n++
	}
	return n
}

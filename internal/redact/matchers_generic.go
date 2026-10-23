package redact

// Builtin rule 25 (generic-api-key), hand-written — one scan over text,
// no regex engine. This is the hot rule: on the Oct 23 Mac measurement the
// generic regex was about 70% of redact time.
//
// It reports exactly the secret-group spans of
//
//	(?i)\b(?:api[_-]?key|access[_-]?token|auth(?:orization)?|bearer|
//	      client[_-]?secret|credential|passwd|password|private[_-]?key|
//	      secret|token)\b["']?<sp>*[:=]<sp>*["']?([0-9A-Za-z_./+=-]{20,})["']?
//
// per the matcher contract in matchers.go (leftmost-first, non-
// overlapping, ASCII \b, Go (?i) folding). The alternation's classes are
// pairwise disjoint, so the only in-alternative backtrack Go regex can
// take is auth(?:orization)? — tried long-form first, then short. Scanning
// resumes after the whole match, whose tail ["']? is consumed but not
// reported.

// m25 is the hand-written matcher for generic-api-key.
func m25(text []byte, out [][2]int) [][2]int {
	n := len(text)
	i := 0
	for i < n {
		end, g0, g1, ok := g25Find(text, i, n)
		if !ok {
			return out
		}
		out = append(out, [2]int{g0, g1})
		i = end
	}
	return out
}

// g25Find scans from byte from for the next genericPattern match and
// returns its end and its secret group span.
func g25Find(text []byte, from, n int) (end, g0, g1 int, ok bool) {
	for p := from; p < n; p++ {
		if !g25StartTab[text[p]] {
			continue // no alternative can start here
		}
		// ASCII \b before the keyword: the first keyword rune is a word
		// character (an ASCII letter) — except U+017F, which folds to s
		// but is not a word character, so there the boundary needs a word
		// character before it instead of a non-word one.
		if text[p] == 0xC5 {
			if p == 0 || !g25WordTab[text[p-1]] {
				continue
			}
		} else if p > 0 && g25WordTab[text[p-1]] {
			continue
		}
		if end, g0, g1, ok := g25MatchAt(text, p, n); ok {
			return end, g0, g1, true
		}
	}
	return 0, 0, 0, false
}

// g25MatchAt tries the alternatives beginning at p in the pattern's order;
// the first whose whole pattern matches wins (leftmost-first).
func g25MatchAt(text []byte, p, n int) (end, g0, g1 int, ok bool) {
	switch c := text[p]; c {
	case 'a', 'A':
		if end, g0, g1, ok = g25TailAt(text, n, g25APIKey(text, p)); ok {
			return end, g0, g1, true
		}
		if end, g0, g1, ok = g25TailAt(text, n, g25AccessToken(text, p)); ok {
			return end, g0, g1, true
		}
		return g25TailAt(text, n, g25Auth(text, p))
	case 'b', 'B':
		return g25TailAt(text, n, g25FoldWord(text, p, "bearer"))
	case 'c', 'C':
		if end, g0, g1, ok = g25TailAt(text, n, g25ClientSecret(text, p)); ok {
			return end, g0, g1, true
		}
		return g25TailAt(text, n, g25FoldWord(text, p, "credential"))
	case 'p', 'P':
		if end, g0, g1, ok = g25TailAt(text, n, g25FoldWord(text, p, "passwd")); ok {
			return end, g0, g1, true
		}
		if end, g0, g1, ok = g25TailAt(text, n, g25FoldWord(text, p, "password")); ok {
			return end, g0, g1, true
		}
		return g25TailAt(text, n, g25PrivateKey(text, p))
	case 's', 'S', 0xC5: // 0xC5 opens U+017F, which folds to s
		return g25TailAt(text, n, g25FoldWord(text, p, "secret"))
	case 't', 'T':
		return g25TailAt(text, n, g25FoldWord(text, p, "token"))
	}
	return 0, 0, 0, false
}

// g25TailAt applies \b after a keyword ending at kwEnd, then the shared
// tail; kwEnd < 0 means the keyword did not match.
func g25TailAt(text []byte, n, kwEnd int) (end, g0, g1 int, ok bool) {
	if kwEnd < 0 || (kwEnd < n && g25WordTab[text[kwEnd]]) {
		return 0, 0, 0, false
	}
	return g25Tail(text, n, kwEnd)
}

// g25Tail matches ["']? <sp>* [:=] <sp>* ["']? <run>{20,} ["']? from q and
// returns the whole-match end and the run span. Greedy and unambiguous:
// the classes are pairwise disjoint, so no backtracking can change a
// verdict; the trailing quote, if present, is consumed by the match but
// lies outside the reported span.
func g25Tail(text []byte, n, q int) (end, g0, g1 int, ok bool) {
	if q < n && (text[q] == '"' || text[q] == '\'') {
		q++
	}
	q = g25SkipSpace(text, q)
	if q >= n || (text[q] != ':' && text[q] != '=') {
		return 0, 0, 0, false
	}
	q++
	q = g25SkipSpace(text, q)
	if q < n && (text[q] == '"' || text[q] == '\'') {
		q++
	}
	r := q
	runes := 0
	for r < n {
		w := g25SecretWidth(text, r)
		if w == 0 {
			break
		}
		r += w
		runes++ // {20,} counts runes, not bytes
	}
	if runes < 20 {
		return 0, 0, 0, false
	}
	end = r
	if end < n && (text[end] == '"' || text[end] == '\'') {
		end++
	}
	return end, q, r, true
}

// The eleven alternatives, in pattern order. Each returns the keyword end
// offset, or -1.

func g25APIKey(text []byte, p int) int {
	e := g25FoldWord(text, p, "api")
	if e < 0 {
		return -1
	}
	return g25FoldWord(text, g25Sep(text, e), "key")
}

func g25AccessToken(text []byte, p int) int {
	e := g25FoldWord(text, p, "access")
	if e < 0 {
		return -1
	}
	return g25FoldWord(text, g25Sep(text, e), "token")
}

func g25ClientSecret(text []byte, p int) int {
	e := g25FoldWord(text, p, "client")
	if e < 0 {
		return -1
	}
	return g25FoldWord(text, g25Sep(text, e), "secret")
}

func g25PrivateKey(text []byte, p int) int {
	e := g25FoldWord(text, p, "private")
	if e < 0 {
		return -1
	}
	return g25FoldWord(text, g25Sep(text, e), "key")
}

// g25Auth matches auth(?:orization)?, long form first — the one
// alternative with a backtrack: if authorization plus tail fails, the
// regex retries without the optional group. When the text really carries
// "authorization", the short form can never pass \b ('o' is a word
// character), so the retry only ever succeeds on plain "auth".
func g25Auth(text []byte, p int) int {
	if e := g25FoldWord(text, p, "authorization"); e >= 0 {
		return e
	}
	return g25FoldWord(text, p, "auth")
}

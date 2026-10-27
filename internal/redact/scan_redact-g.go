package redact

// Byte-level scanners for the hand-written generic-api-key matcher.
// Everything here copies Go regexp semantics for genericPattern under
// (?i), which the frozen oracle runs: ASCII \b (word = [0-9A-Za-z_]),
// leftmost-first alternation, and Unicode simple-case folding — so the
// letters k and s also fold to U+212A (KELVIN SIGN) and U+017F (LATIN
// SMALL LETTER LONG S), in keywords and in folded character classes.

var (
	g25StartTab = func() [256]bool {
		var t [256]bool
		for _, c := range "aAbBcCpPsStT" {
			t[c] = true
		}
		t[0xC5] = true
		return t
	}()

	g25WordTab = func() [256]bool {
		var t [256]bool
		for c := byte('0'); c <= '9'; c++ {
			t[c] = true
		}
		for c := byte('A'); c <= 'Z'; c++ {
			t[c] = true
		}
		for c := byte('a'); c <= 'z'; c++ {
			t[c] = true
		}
		t['_'] = true
		return t
	}()

	g25SecretTab = func() [256]bool {
		var t [256]bool
		for _, c := range "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_./+=-" {
			t[c] = true
		}
		return t
	}()
)

// g25FoldWidth reports how many bytes at text[i] fold to the ASCII letter
// c under Go regexp (?i) simple folding: 1 for either case, 3 for U+212A
// folding to k, 2 for U+017F folding to s, 0 when none matches.
func g25FoldWidth(text []byte, i int, c byte) int {
	if i >= len(text) {
		return 0
	}
	if b := text[i]; b == c || b == c^0x20 {
		return 1
	}
	switch c {
	case 'k':
		if b := text[i]; b == 0xE2 && i+2 < len(text) && text[i+1] == 0x84 && text[i+2] == 0xAA {
			return 3
		}
	case 's':
		if b := text[i]; b == 0xC5 && i+1 < len(text) && text[i+1] == 0xBF {
			return 2
		}
	}
	return 0
}

func g25FoldWord(text []byte, i int, w string) int {
	p := i
	for k := 0; k < len(w); k++ {
		n := g25FoldWidth(text, p, w[k])
		if n == 0 {
			return -1
		}
		p += n
	}
	return p
}

func g25Sep(text []byte, p int) int {
	if p < len(text) && (text[p] == '_' || text[p] == '-') {
		return p + 1
	}
	return p
}

func g25SecretWidth(text []byte, i int) int {
	if g25SecretTab[text[i]] {
		return 1
	}
	switch text[i] {
	case 0xC5:
		if i+1 < len(text) && text[i+1] == 0xBF {
			return 2
		}
	case 0xE2:
		if i+2 < len(text) && text[i+1] == 0x84 && text[i+2] == 0xAA {
			return 3
		}
	}
	return 0
}

// g25SkipSpace consumes the maximal run of rustSpace runes at text[i]
// (U+0085, U+00A0, U+1680, U+2000-U+200A, U+2028, U+2029, U+202F, U+205F,
// U+3000 and the six ASCII spaces). Invalid UTF-8 is never rustSpace and
// steps one byte, as Go regexp stepping one U+FFFD would.
func g25SkipSpace(text []byte, i int) int {
	n := len(text)
	for i < n {
		b := text[i]
		switch {
		case b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r':
			i++
		case b == 0xC2 && i+1 < n && (text[i+1] == 0x85 || text[i+1] == 0xA0):
			i += 2
		case b == 0xE1 && i+2 < n && text[i+1] == 0x9A && text[i+2] == 0x80:
			i += 3
		case b == 0xE2 && i+2 < n && text[i+1] == 0x80 &&
			(text[i+2] >= 0x80 && text[i+2] <= 0x8A || text[i+2] == 0xA8 || text[i+2] == 0xA9 || text[i+2] == 0xAF):
			i += 3
		case b == 0xE2 && i+2 < n && text[i+1] == 0x81 && text[i+2] == 0x9F:
			i += 3
		case b == 0xE3 && i+2 < n && text[i+1] == 0x80 && text[i+2] == 0x80:
			i += 3
		default:
			return i
		}
	}
	return i
}

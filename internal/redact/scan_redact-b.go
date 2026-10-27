package redact

import "bytes"

func classDigit(c byte) bool { return '0' <= c && c <= '9' }

func wordStart(text []byte, s int) bool {
	return s == 0 || !isWordByte(text[s-1])
}

func tailEnd(text []byte, i, min int, class classFn) int {
	e := classRunEnd(text, i, class)
	if e-i < min {
		return -1
	}
	for p := e; ; p-- {
		if wordBoundary(text, p) {
			return p
		}
		if p == i+min {
			return -1
		}
	}
}

func tailRule(pat string, min int, class classFn) matcher {
	p := []byte(pat)
	return func(text []byte, out [][2]int) [][2]int {
		for i := 0; i < len(text); {
			s := bytes.Index(text[i:], p)
			if s < 0 {
				return out
			}
			s += i
			if wordStart(text, s) {
				if e := tailEnd(text, s+len(p), min, class); e >= 0 {
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

func hexRule(pat string, n int) matcher {
	p := []byte(pat)
	return func(text []byte, out [][2]int) [][2]int {
		for i := 0; i < len(text); {
			s := bytes.Index(text[i:], p)
			if s < 0 {
				return out
			}
			s += i
			e := s + len(p) + n
			if e <= len(text) && wordStart(text, s) && lowerHex(text[s+len(p):e]) && (e == len(text) || !isWordByte(text[e])) {
				out = append(out, [2]int{s, e})
				i = e
				continue
			}
			i = s + 1
		}
		return out
	}
}

func lowerHex(v []byte) bool {
	for _, c := range v {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

var openAIKey = func(text []byte, out [][2]int) [][2]int {
	const proj = "proj-"
	p := []byte("sk-")
	for i := 0; i < len(text); {
		s := bytes.Index(text[i:], p)
		if s < 0 {
			return out
		}
		s += i
		if wordStart(text, s) {
			e := -1
			if bytes.HasPrefix(text[s+3:], []byte(proj)) {
				e = tailEnd(text, s+3+len(proj), 20, classWordDash)
			}
			if e < 0 {
				e = tailEnd(text, s+len(p), 20, classWordDash)
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

var sendGridKey = func(text []byte, out [][2]int) [][2]int {
	p := []byte("SG.")
	for i := 0; i < len(text); {
		s := bytes.Index(text[i:], p)
		if s < 0 {
			return out
		}
		s += i
		if wordStart(text, s) {
			e1 := classRunEnd(text, s+len(p), classWordDash)
			if e1-(s+len(p)) >= 20 && e1 < len(text) && text[e1] == '.' {
				if e := tailEnd(text, e1+1, 20, classWordDash); e >= 0 {
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

var discordWebhook = func(text []byte, out [][2]int) [][2]int {
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
			e, ok = hookTail(text, h+len(app), mid)
		}
		if !ok {
			e, ok = hookTail(text, h, mid)
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

func hookTail(text []byte, h int, mid string) (int, bool) {
	if !bytes.HasPrefix(text[h:], []byte(mid)) {
		return 0, false
	}
	d := classRunEnd(text, h+len(mid), classDigit)
	if d-(h+len(mid)) < 6 || d >= len(text) || text[d] != '/' {
		return 0, false
	}
	e := classRunEnd(text, d+1, classWordDash)
	if e-(d+1) < 20 {
		return 0, false
	}
	return e, true
}

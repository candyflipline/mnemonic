package redact

import "regexp"

type matcher func(text []byte, out [][2]int) [][2]int

func regexMatcher(re *regexp.Regexp, group int) matcher {
	return func(text []byte, out [][2]int) [][2]int {
		for _, loc := range re.FindAllSubmatchIndex(text, -1) {
			g0, g1 := loc[2*group], loc[2*group+1]
			if g0 < 0 {
				continue
			}
			out = append(out, [2]int{g0, g1})
		}
		return out
	}
}

type tokenRule struct {
	boundaryBefore bool
	head           prefixFn
	class          classFn
	minRun, maxRun int
	boundaryAfter  bool
}

func (r tokenRule) match(text []byte, out [][2]int) [][2]int {
	return scanToken(text, out, r)
}

func pemWithKind(begin, end, tail string) matcher {
	b, e, t := []byte(begin), []byte(end), []byte(tail)
	return func(text []byte, out [][2]int) [][2]int {
		return scanPEM(text, out, b, e, true, t)
	}
}

func pemLiteral(begin, end string) matcher {
	b, e := []byte(begin), []byte(end)
	return func(text []byte, out [][2]int) [][2]int {
		return scanPEM(text, out, b, e, false, nil)
	}
}

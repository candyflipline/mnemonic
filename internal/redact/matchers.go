package redact

import "regexp"

// matcher appends the match spans of one builtin rule over text, in out,
// and returns the extended slice. The contract, which both the regex-backed
// entries and the future hand-written ones must meet:
//
//   - Semantics are exactly those of the rule's regex reporting matches via
//     FindAllSubmatchIndex(text, -1): leftmost-first, non-overlapping,
//     successive matches, in report order — so spans come back ascending
//     and pairwise disjoint.
//   - Each appended span is the span of the rule's secret group
//     (builtinRules[i].secretGroup); matches whose secret group did not
//     participate are skipped.
//   - Word boundaries are ASCII \b, as in the builtin patterns.
//   - text must not be retained or modified.
//   - A matcher is pure and holds no mutable shared state: Line, and hence
//     any matcher, may run on many goroutines at once.
//   - Filtering (existing [REDACTED:...] placeholders, UTF-8 validity,
//     minLen, marker, entropy gate) happens outside, in
//     nonOverlappingSpans; a matcher reports what its regex would.
//
// The entries all start regex-backed (regexEntry); lanes U2b, U2c and U2d
// replace them with hand-written matchers, one file each, meeting this
// contract, proven by TestFuzzAgainstOracle.
type matcher func(text []byte, out [][2]int) [][2]int

// matchers is indexed by builtin rule index, i.e. builtinRules order,
// 0-25. Extra config patterns are not in this table; they stay regexes.
var matchers = [26]matcher{
	m00, m01, m02, m03, m04, m05, m06, m07, m08, m09, m10, m11, m12,
	m13, m14, m15, m16, m17, m18, m19, m20, m21, m22, m23, m24, m25,
}

func regexEntry(i int) matcher {
	return regexMatcher(regexp.MustCompile(builtinRules[i].pattern), builtinRules[i].secretGroup)
}

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

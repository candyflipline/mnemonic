package redact

type keywordAutomaton struct {
	transition    [][256]int32
	nearestOutput []int32
	rulesEndingAt [][]int32
}

func buildKeywordAutomaton(rules []rule) *keywordAutomaton {
	type node struct {
		kids map[byte]int32
	}
	nodes := []node{{kids: map[byte]int32{}}}
	ends := [][]int32{{}}
	for i := range rules {
		for _, kw := range rules[i].keywords {
			if len(kw) == 0 {
				continue
			}
			s := int32(0)
			for _, b := range kw {
				nx, ok := nodes[s].kids[b]
				if !ok {
					nx = int32(len(nodes))
					nodes = append(nodes, node{kids: map[byte]int32{}})
					ends = append(ends, nil)
					nodes[s].kids[b] = nx
				}
				s = nx
			}
			ends[s] = append(ends[s], int32(i))
		}
	}
	n := len(nodes)
	fail := make([]int32, n)
	nearestOutput := make([]int32, n)
	nearestOutput[0] = -1
	order := []int32{0}
	for qi := 0; qi < len(order); qi++ {
		s := order[qi]
		for b, nx := range nodes[s].kids {
			if s == 0 {
				fail[nx] = 0
			} else {
				f := fail[s]
				for f != 0 {
					if nf, ok := nodes[f].kids[b]; ok {
						f = nf
						break
					}
					f = fail[f]
				}
				if f == 0 {
					if nf, ok := nodes[0].kids[b]; ok {
						f = nf
					}
				}
				fail[nx] = f
			}
			if len(ends[fail[nx]]) > 0 {
				nearestOutput[nx] = fail[nx]
			} else {
				nearestOutput[nx] = nearestOutput[fail[nx]]
			}
			order = append(order, nx)
		}
	}
	transition := make([][256]int32, n)
	for _, s := range order {
		row := &transition[s]
		for b := 0; b < 256; b++ {
			if nx, ok := nodes[s].kids[byte(b)]; ok {
				row[b] = nx
			} else if s == 0 {
				row[b] = 0
			} else {
				row[b] = transition[fail[s]][b]
			}
		}
	}
	rulesEndingAt := make([][]int32, n)
	for s := range ends {
		if len(ends[s]) > 0 {
			rulesEndingAt[s] = ends[s]
		}
	}
	return &keywordAutomaton{transition: transition, nearestOutput: nearestOutput, rulesEndingAt: rulesEndingAt}
}

func (a *keywordAutomaton) scan(text []byte, c []bool) {
	s := int32(0)
	for _, b := range text {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		s = a.transition[s][b]
		for o := s; o >= 0; o = a.nearestOutput[o] {
			for _, ri := range a.rulesEndingAt[o] {
				c[ri] = true
			}
		}
	}
}

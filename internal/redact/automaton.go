package redact

// keywordAutomaton is an Aho-Corasick automaton over ASCII-folded bytes.
// scan folds 'A'-'Z' exactly like asciiLower and nothing else, so it marks
// exactly the rules whose keywords bytes.Contains would find in
// asciiLower(text) — in one pass over the text, with no lowered copy.
// Built once in New and never mutated afterwards, so Line stays safe to
// call from many goroutines.
type keywordAutomaton struct {
	next []int32   // next[state*256+byte] -> next state, fail transitions baked in
	out  []int32   // nearest state on the suffix chain that ends a keyword, -1 if none
	kr   [][]int32 // kr[state] -> rule indices whose keyword ends at that state
}

func buildKeywordAutomaton(rules []rule) *keywordAutomaton {
	type node struct {
		kids map[byte]int32
	}
	nodes := []node{{kids: map[byte]int32{}}}
	ends := [][]int32{{}} // ends[i]: rule indices whose keyword ends at node i
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
	out := make([]int32, n)
	out[0] = -1
	order := []int32{0} // BFS order: fail[s] and out[s] are set before s is left
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
				out[nx] = fail[nx]
			} else {
				out[nx] = out[fail[nx]]
			}
			order = append(order, nx)
		}
	}
	next := make([]int32, n*256)
	for _, s := range order {
		row := next[int(s)*256 : int(s)*256+256]
		for b := 0; b < 256; b++ {
			if nx, ok := nodes[s].kids[byte(b)]; ok {
				row[b] = nx
			} else if s == 0 {
				row[b] = 0
			} else {
				row[b] = next[int(fail[s])*256+b]
			}
		}
	}
	kr := make([][]int32, n)
	for s := range ends {
		if len(ends[s]) > 0 {
			kr[s] = ends[s]
		}
	}
	return &keywordAutomaton{next: next, out: out, kr: kr}
}

func (a *keywordAutomaton) scan(text []byte, c []bool) {
	s := int32(0)
	for _, b := range text {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		s = a.next[int(s)*256+int(b)]
		for o := s; o >= 0; o = a.out[o] { // kr[s] first, then proper suffix outputs
			for _, ri := range a.kr[o] {
				c[ri] = true
			}
		}
	}
}

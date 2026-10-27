package index

import "iter"

type borrowedRow Row

type sessionView struct {
	evs       []built
	seqs      []uint32
	cur       built
	payloadOf func(built) string
}

func (v *sessionView) withSeqs() iter.Seq2[*borrowedRow, uint32] {
	return func(yield func(*borrowedRow, uint32) bool) {
		for i, b := range v.evs {
			v.cur = b
			if !yield((*borrowedRow)(b.row), v.seqs[i]) {
				return
			}
		}
	}
}

func (v *sessionView) rows() iter.Seq[*borrowedRow] {
	return func(yield func(*borrowedRow) bool) {
		var scratch Row
		for i, b := range v.evs {
			v.cur = b
			scratch = *b.row
			scratch.TurnIndex = uint64(v.seqs[i])
			if !yield((*borrowedRow)(&scratch)) {
				return
			}
		}
	}
}

func (v *sessionView) payload(*borrowedRow) string { return v.payloadOf(v.cur) }

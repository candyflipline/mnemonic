package index

import "iter"

// sessionView hands a session's materialized winners to the derivations as
// the iterators they take, until the streaming build reads them from the
// part's own logs. Rows yielded by rows are a scratch copy with TurnIndex set
// from the session's turn seqs; withSeqs yields the winners themselves with
// their seq beside them. payload answers for the row being yielded.
type sessionView struct {
	evs       []built
	seqs      []uint32
	cur       built
	payloadOf func(built) string
}

func (v *sessionView) withSeqs() iter.Seq2[*Row, uint32] {
	return func(yield func(*Row, uint32) bool) {
		for i, b := range v.evs {
			v.cur = b
			if !yield(b.row, v.seqs[i]) {
				return
			}
		}
	}
}

func (v *sessionView) rows() iter.Seq[*Row] {
	return func(yield func(*Row) bool) {
		var scratch Row
		for i, b := range v.evs {
			v.cur = b
			scratch = *b.row
			scratch.TurnIndex = uint64(v.seqs[i])
			if !yield(&scratch) {
				return
			}
		}
	}
}

func (v *sessionView) payload(*Row) string { return v.payloadOf(v.cur) }

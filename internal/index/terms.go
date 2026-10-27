package index

import (
	"bytes"
	"encoding/binary"
	"sort"
)

const (
	termBlock  = 16
	filterBits = 2048 * 8
)

func encodeTerms(terms []string, offs, lens []uint64) (dictB, idx, filter []byte) {
	filter = make([]byte, filterBits/8)
	prev := ""
	for i, t := range terms {
		shared := 0
		if i%termBlock == 0 {
			idx = binary.LittleEndian.AppendUint32(idx, uint32(len(dictB)))
		} else {
			for shared < len(t) && shared < len(prev) && t[shared] == prev[shared] {
				shared++
			}
		}
		dictB = binary.AppendUvarint(dictB, uint64(shared))
		dictB = binary.AppendUvarint(dictB, uint64(len(t)-shared))
		dictB = append(dictB, t[shared:]...)
		dictB = binary.AppendUvarint(dictB, offs[i])
		dictB = binary.AppendUvarint(dictB, lens[i])
		for _, bit := range filterProbes(t) {
			filter[bit>>3] |= 1 << (bit & 7)
		}
		prev = t
	}
	return dictB, idx, filter
}

func filterProbes(t string) [3]uint32 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(t); i++ {
		h = (h ^ uint64(t[i])) * 1099511628211
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	step := (h >> 32) | 1
	var out [3]uint32
	for i := range out {
		out[i] = uint32(h % filterBits)
		h += step
	}
	return out
}

func (st *Part) postingsOf(term string) []byte {
	for _, bit := range filterProbes(term) {
		if st.termFilter[bit>>3]&(1<<(bit&7)) == 0 {
			return nil
		}
	}
	nb := len(st.termIdx) / 4
	want := []byte(term)
	b := sort.Search(nb, func(i int) bool {
		p := st.terms[binary.LittleEndian.Uint32(st.termIdx[i*4:]):]
		_, k := binary.Uvarint(p)
		l, k2 := binary.Uvarint(p[k:])
		return bytes.Compare(p[k+k2:k+k2+int(l)], want) > 0
	}) - 1
	if b < 0 {
		return nil
	}
	p := st.terms[binary.LittleEndian.Uint32(st.termIdx[b*4:]):]
	var buf [64]byte
	n := 0
	for i := 0; i < termBlock && len(p) > 0; i++ {
		shared, k := binary.Uvarint(p)
		p = p[k:]
		l, k := binary.Uvarint(p)
		p = p[k:]
		n = int(shared) + copy(buf[shared:], p[:l])
		p = p[l:]
		off, k := binary.Uvarint(p)
		p = p[k:]
		ln, k := binary.Uvarint(p)
		p = p[k:]
		switch bytes.Compare(buf[:n], want) {
		case 0:
			return st.postings[off : off+ln]
		case 1:
			return nil
		}
	}
	return nil
}

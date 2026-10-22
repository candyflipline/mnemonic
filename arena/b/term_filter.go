package main

// Fixed-size per-segment negative filters stay on the heap. They let absent
// terms avoid cold dictionary page faults without retaining a per-term heap
// dictionary. False positives only cause an ordinary exact dictionary lookup.
func termHash(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h = (h ^ uint64(c)) * 1099511628211
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	return h ^ (h >> 33)
}
func (s *segment) mayContain(h uint64) bool {
	step := (h >> 32) | 1
	for i := 0; i < 3; i++ {
		bit := h & 16383
		if s.filter[bit>>6]&(uint64(1)<<(bit&63)) == 0 {
			return false
		}
		h += step
	}
	return true
}
func (s *segment) buildFilter() {
	pos := 8
	n, _ := readUAt(s.postings, &pos)
	var term [64]byte
	for i := uint64(0); i < n; i++ {
		prefix, _ := readUAt(s.postings, &pos)
		size, _ := readUAt(s.postings, &pos)
		copy(term[prefix:], s.postings[pos:pos+int(size)])
		pos += int(size)
		length := int(prefix + size)
		count, _ := readUAt(s.postings, &pos)
		pos += int(count)
		h := termHash(term[:length])
		step := (h >> 32) | 1
		for j := 0; j < 3; j++ {
			bit := h & 16383
			s.filter[bit>>6] |= uint64(1) << (bit & 63)
			h += step
		}
	}
	s.filterReady = true
}

package push

import (
	"bufio"
	"bytes"
	"io"
	"os"

	"github.com/candyflipline/mnemonic/internal/wire"
)

// redactComplete reads path and returns every whole line of it, redacted:
// bytes up to and including the last '\n', with each line redacted and
// re-terminated so the wire keeps its one-line-per-line framing. The hash of
// the first redacted line comes back too, since the manifest is keyed on it,
// and rawEnds holds the offset in path after each line — line i's redacted
// bytes end at the i-th '\n' of out and its raw bytes at rawEnds[i], so a
// body cut on a redacted boundary knows the raw_end to send with it.
// ok is false when the file has no whole line yet; its partial tail waits for
// a later pass.
func redactComplete(path string, red Redactor) (out []byte, head string, rawEnds []int64, ok bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", nil, false, err
	}
	end := bytes.LastIndexByte(raw, '\n')
	if end < 0 {
		return nil, "", nil, false, nil
	}
	out = make([]byte, 0, end+1)
	rawEnds = make([]int64, 0, 64)
	headLen := -1
	for rest, at := raw[:end+1], int64(0); len(rest) > 0; {
		nl := bytes.IndexByte(rest, '\n') + 1 // whole content ends in '\n', so nl > 0
		// The terminator is held back from Line: a rule anchored at end of
		// line must see end of input, and a replacement is documented never
		// to introduce '\n', so re-appending it cannot corrupt a line.
		out = append(out, red.Line(rest[:nl-1])...)
		out = append(out, '\n')
		at += int64(nl)
		rawEnds = append(rawEnds, at)
		if headLen < 0 {
			headLen = len(out)
		}
		rest = rest[nl:]
	}
	return out, wire.HeadHash(out[:headLen]), rawEnds, true, nil
}

// redactTail verifies a manifest entry against the file's first line and
// redacts only the raw bytes past entry.RawEnd, the same way redactComplete
// does. It returns that redacted tail with each line's ending raw offset, and
// the head hash the verification used. ok is false when the fast path's
// premise does not hold — the file shrank below RawEnd, has no whole first
// line, or RawEnd is not where the server's copy ends — and the caller must
// take the full path.
func redactTail(path string, red Redactor, entry *wire.Source) (tail []byte, head string, rawEnds []int64, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", nil, false, err
	}
	if info.Size() < entry.RawEnd {
		return nil, "", nil, false, nil
	}
	// The head hash is the only proof the server's copy starts where ours
	// does, so the first line is read and redacted in full for it.
	first, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		if err == io.EOF { // no whole line yet
			return nil, "", nil, false, nil
		}
		return nil, "", nil, false, err
	}
	red1 := red.Line(first[:len(first)-1])
	headLine := make([]byte, 0, len(red1)+1)
	headLine = append(headLine, red1...)
	headLine = append(headLine, '\n')
	head = wire.HeadHash(headLine)
	if head != entry.HeadHash {
		return nil, "", nil, false, nil
	}
	var one [1]byte
	if _, err := f.ReadAt(one[:], entry.RawEnd-1); err != nil || one[0] != '\n' {
		return nil, "", nil, false, nil
	}
	rest := io.NewSectionReader(f, entry.RawEnd, info.Size()-entry.RawEnd)
	sc := bufio.NewReader(rest)
	tail = make([]byte, 0, info.Size()-entry.RawEnd)
	rawEnds = make([]int64, 0, 64)
	at := entry.RawEnd
	for {
		line, err := sc.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			tail = append(tail, red.Line(line[:len(line)-1])...)
			tail = append(tail, '\n')
			at += int64(len(line))
			rawEnds = append(rawEnds, at)
			continue
		}
		if err != nil && err != io.EOF {
			return nil, "", nil, false, err
		}
		break // a partial last line waits for the next pass, as redactComplete leaves it
	}
	return tail, head, rawEnds, true, nil
}

// cutAtSize reports whether size falls on a redacted line boundary and, when
// it does, the offset of the first byte after it — the only safe seam to
// append from.
func cutAtSize(redacted []byte, size int64) (cut int64, ok bool) {
	for cut = 0; cut < int64(len(redacted)); {
		cut += int64(bytes.IndexByte(redacted[cut:], '\n')) + 1
		if cut == size {
			return cut, true
		}
		if cut > size {
			return 0, false
		}
	}
	return 0, false
}

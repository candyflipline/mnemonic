package push

import (
	"bytes"
	"os"

	"github.com/candyflipline/mnemonic/internal/wire"
)

// redactComplete reads path and returns every whole line of it, redacted:
// bytes up to and including the last '\n', with each line redacted and
// re-terminated so the wire keeps its one-line-per-line framing. The hash of
// the first redacted line comes back too, since the manifest is keyed on it.
// ok is false when the file has no whole line yet; its partial tail waits for
// a later pass.
func redactComplete(path string, red Redactor) (out []byte, head string, ok bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", false, err
	}
	end := bytes.LastIndexByte(raw, '\n')
	if end < 0 {
		return nil, "", false, nil
	}
	out = make([]byte, 0, end+1)
	headLen := -1
	for rest := raw[:end+1]; len(rest) > 0; {
		nl := bytes.IndexByte(rest, '\n') + 1 // whole content ends in '\n', so nl > 0
		// The terminator is held back from Line: a rule anchored at end of
		// line must see end of input, and a replacement is documented never
		// to introduce '\n', so re-appending it cannot corrupt a line.
		out = append(out, red.Line(rest[:nl-1])...)
		out = append(out, '\n')
		if headLen < 0 {
			headLen = len(out)
		}
		rest = rest[nl:]
	}
	return out, wire.HeadHash(out[:headLen]), true, nil
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

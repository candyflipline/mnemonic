package push

import (
	"bufio"
	"bytes"
	"io"
	"os"

	"github.com/candyflipline/mnemonic/internal/wire"
)

type redactedLines struct {
	body    []byte
	rawEnds []int64
	head    string
}

func redactLines(red Redactor, raw []byte, base int64) redactedLines {
	r := redactedLines{body: make([]byte, 0, len(raw))}
	for at := base; ; {
		nl := bytes.IndexByte(raw, '\n')
		if nl < 0 {
			return r
		}
		r.body = append(r.body, red.Line(raw[:nl])...)
		r.body = append(r.body, '\n')
		at += int64(nl + 1)
		r.rawEnds = append(r.rawEnds, at)
		raw = raw[nl+1:]
	}
}

func redactComplete(path string, red Redactor) (*redactedLines, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := redactLines(red, raw, 0)
	if len(r.rawEnds) == 0 {
		return nil, nil
	}
	r.head = wire.HeadHash(r.body[:bytes.IndexByte(r.body, '\n')+1])
	return &r, nil
}

func redactTail(path string, red Redactor, entry *wire.Source) (*redactedLines, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < entry.RawEnd {
		return nil, nil
	}
	first, err := bufio.NewReader(f).ReadBytes('\n')
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	head := wire.HeadHash(redactLines(red, first, 0).body)
	if head != entry.HeadHash {
		return nil, nil
	}
	var one [1]byte
	if _, err := f.ReadAt(one[:], entry.RawEnd-1); err != nil || one[0] != '\n' {
		return nil, nil
	}
	raw := make([]byte, info.Size()-entry.RawEnd)
	n, err := f.ReadAt(raw, entry.RawEnd)
	if err != nil && err != io.EOF {
		return nil, err
	}
	r := redactLines(red, raw[:n], entry.RawEnd)
	r.head = head
	return &r, nil
}

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

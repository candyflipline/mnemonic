package main

import (
	"bufio"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/klauspost/compress/zstd"
)

const mergeFanout = 8
const maxMergeTier = 3
const maxMergeBytes = 2 << 20

type mergeInfo struct {
	tier  int
	bytes int64
}

func inspectMerge(dir, name string) (mergeInfo, error) {
	var out mergeInfo
	for _, file := range []string{"meta.zst", "heavy", "terms"} {
		st, err := os.Stat(filepath.Join(dir, name, file))
		if err != nil {
			return out, err
		}
		out.bytes += st.Size()
	}
	b, err := os.ReadFile(filepath.Join(dir, name, "tier"))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if len(b) != 1 || int(b[0]) > maxMergeTier {
		return out, fmt.Errorf("invalid segment tier")
	}
	out.tier = int(b[0])
	return out, nil
}

// Only consecutive same-tier intervals of one source merge. This preserves
// equal-version row order. Three promotions bound each event to four derived
// writes including the initial one. Inputs above the byte cap remain untouched.
func compactSourceSegments(dir string, names []string, codecs *segmentCodecs) ([]string, error) {
	if len(names) < mergeFanout {
		return names, nil
	}
	info := map[string]mergeInfo{}
	for {
		merged := false
		for start := 0; start+mergeFanout <= len(names); start++ {
			tier := -1
			size := int64(0)
			same := true
			for _, name := range names[start : start+mergeFanout] {
				item, ok := info[name]
				if !ok {
					var err error
					item, err = inspectMerge(dir, name)
					if err != nil {
						return nil, err
					}
					info[name] = item
				}
				if tier < 0 {
					tier = item.tier
				}
				same = same && item.tier == tier
				size += item.bytes
			}
			if !same || tier >= maxMergeTier || size > maxMergeBytes {
				continue
			}
			name, err := mergeSegments(dir, names[start:start+mergeFanout], tier+1, codecs)
			if err != nil {
				return nil, err
			}
			next := make([]string, 0, len(names)-mergeFanout+1)
			next = append(next, names[:start]...)
			next = append(next, name)
			next = append(next, names[start+mergeFanout:]...)
			names = next
			merged = true
			break
		}
		if !merged {
			return names, nil
		}
	}
}

func mergeSegments(dir string, names []string, tier int, codecs *segmentCodecs) (_ string, err error) {
	w, err := newSegmentWriter(dir, codecs)
	if err != nil {
		return "", err
	}
	w.tier = tier
	defer func() {
		if err != nil {
			w.abort()
		}
	}()
	for _, name := range names {
		path := filepath.Join(dir, name)
		baseDoc, baseOffset := w.count, w.heavyOffset
		heavy, e := os.Open(filepath.Join(path, "heavy"))
		if e != nil {
			return "", e
		}
		n, e := io.Copy(w.heavyBuffer, heavy)
		heavy.Close()
		if e != nil {
			return "", e
		}
		w.heavyOffset += uint64(n)
		f, e := os.Open(filepath.Join(path, "meta.zst"))
		if e != nil {
			return "", e
		}
		zr, e := zstd.NewReader(bufio.NewReaderSize(f, 64<<10), zstd.WithDecoderConcurrency(1))
		if e != nil {
			f.Close()
			return "", e
		}
		dec := gob.NewDecoder(zr)
		for {
			var ev event
			e = dec.Decode(&ev)
			if e == io.EOF {
				break
			}
			if e != nil {
				zr.Close()
				f.Close()
				return "", e
			}
			ev.HeavyOffset += baseOffset
			if e = w.enc.Encode(&ev); e != nil {
				zr.Close()
				f.Close()
				return "", e
			}
			w.count++
		}
		zr.Close()
		f.Close()
		_, raw, e := loadTerms(filepath.Join(path, "terms"))
		if e != nil {
			return "", e
		}
		e = mergeTerms(w, raw, baseDoc, w.count-baseDoc)
		syscall.Munmap(raw)
		if e != nil {
			return "", e
		}
	}
	return w.finish()
}
func mergeTerms(w *segmentWriter, raw []byte, base, count uint32) error {
	pos := 8
	n, err := readUAt(raw, &pos)
	if err != nil {
		return err
	}
	var previous [64]byte
	length := 0
	for i := uint64(0); i < n; i++ {
		prefix, _ := readUAt(raw, &pos)
		size, _ := readUAt(raw, &pos)
		copy(previous[prefix:], raw[pos:pos+int(size)])
		length = int(prefix + size)
		pos += int(size)
		bytes, _ := readUAt(raw, &pos)
		postings := raw[pos : pos+int(bytes)]
		pos += int(bytes)
		term := string(previous[:length])
		out := w.terms[term]
		doc := uint32(0)
		for len(postings) > 0 {
			gap, k := binary.Uvarint(postings)
			if k <= 0 || gap > uint64(^uint32(0)-doc) {
				return fmt.Errorf("invalid merge posting")
			}
			postings = postings[k:]
			doc += uint32(gap)
			tf, k := binary.Uvarint(postings)
			if k <= 0 || tf > uint64(^uint32(0)) || doc >= count {
				return fmt.Errorf("invalid merge frequency")
			}
			postings = postings[k:]
			out = append(out, posting{base + doc, uint32(tf)})
		}
		w.terms[term] = out
	}
	return nil
}

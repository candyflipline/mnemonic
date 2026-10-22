package trace

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const (
	packAlign = 4096
	packMagic = "mnpack01"
)

// packIndex is the footer of a packed part: every member's place in the blob.
type packIndex struct {
	Members []packMember `json:"members"`
}

type packMember struct {
	Name string `json:"name"`
	Off  int64  `json:"off"`
	Size int64  `json:"size"`
}

type packWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (p *packWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.h.Write(b[:n])
	p.n += int64(n)
	return n, err
}

// Pack writes the part dir as one blob: each file at a 4 KiB-aligned offset,
// then the JSON index, its length as u64 LE, and the magic.
func Pack(partDir string, w io.Writer) (sha256hex string, size int64, err error) {
	ents, err := os.ReadDir(partDir)
	if err != nil {
		return "", 0, err
	}
	pw := &packWriter{w: w, h: sha256.New()}
	var idx packIndex
	for _, e := range ents {
		if !e.Type().IsRegular() {
			return "", 0, fmt.Errorf("pack %s: %s is not a regular file", partDir, e.Name())
		}
		if pad := (packAlign - pw.n%packAlign) % packAlign; pad > 0 {
			if _, err := pw.Write(make([]byte, pad)); err != nil {
				return "", 0, err
			}
		}
		off := pw.n
		f, err := os.Open(filepath.Join(partDir, e.Name()))
		if err != nil {
			return "", 0, err
		}
		n, err := io.Copy(pw, f)
		f.Close()
		if err != nil {
			return "", 0, err
		}
		idx.Members = append(idx.Members, packMember{Name: e.Name(), Off: off, Size: n})
	}
	ib, err := json.Marshal(idx)
	if err != nil {
		return "", 0, err
	}
	var tail [8]byte
	binary.LittleEndian.PutUint64(tail[:], uint64(len(ib)))
	for _, b := range [][]byte{ib, tail[:], []byte(packMagic)} {
		if _, err := pw.Write(b); err != nil {
			return "", 0, err
		}
	}
	return hex.EncodeToString(pw.h.Sum(nil)), pw.n, nil
}

// OpenPacked mmaps a blob Pack wrote and opens the part its members make up.
func OpenPacked(path string) (*Part, error) {
	blob, err := mmapFile(path)
	if err != nil {
		return nil, err
	}
	members, err := readPackIndex(blob)
	if err != nil {
		if len(blob) > 0 {
			syscall.Munmap(blob)
		}
		return nil, fmt.Errorf("open packed %s: %w", path, err)
	}
	member := func(name string) ([]byte, error) {
		b, ok := members[name]
		if !ok {
			return nil, fmt.Errorf("open packed %s: %s: %w", path, name, fs.ErrNotExist)
		}
		return b, nil
	}
	meta, err := member("meta.json")
	if err != nil {
		syscall.Munmap(blob)
		return nil, err
	}
	st, err := openPart(meta, member, member)
	st.maps = [][]byte{blob}
	if err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

func readPackIndex(blob []byte) (map[string][]byte, error) {
	tl := int64(8 + len(packMagic))
	end := int64(len(blob)) - tl
	if end < 0 || !bytes.Equal(blob[end+8:], []byte(packMagic)) {
		return nil, fmt.Errorf("not a packed part")
	}
	il := int64(binary.LittleEndian.Uint64(blob[end:]))
	if il < 0 || il > end {
		return nil, fmt.Errorf("index length %d out of range", il)
	}
	var idx packIndex
	if err := json.Unmarshal(blob[end-il:end], &idx); err != nil {
		return nil, err
	}
	members := make(map[string][]byte, len(idx.Members))
	for _, m := range idx.Members {
		if m.Off < 0 || m.Size < 0 || m.Off%packAlign != 0 || m.Off+m.Size > end-il {
			return nil, fmt.Errorf("member %s out of range", m.Name)
		}
		members[m.Name] = blob[m.Off : m.Off+m.Size : m.Off+m.Size]
	}
	return members, nil
}

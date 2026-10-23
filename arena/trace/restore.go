package trace

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrNoParseRecord: the part predates partFormat 4, so its members parse from
// raw.
var ErrNoParseRecord = errors.New("trace: part has no parse records")

// RestorePart makes store cover part id as the keeper that built it did:
// parts/<id>/ unpacked from the blob fetch streams (checked against
// sha256hex, skipped when the dir exists), and for every member with no
// checkpoint, logs re-framed from the part's own frames plus a checkpoint
// marked restored. It reports how many members it restored. Safe to rerun
// after a crash at any point.
func RestorePart(store, id, sha256hex string, fetch func(io.Writer) error) (restored int, err error) {
	dir := partDir(store, id)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := fetchPart(store, id, sha256hex, fetch); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	st, err := OpenPartDir(dir)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	if err := parseRecords(st.meta.Members); err != nil {
		return 0, fmt.Errorf("part %s: %w", id, err)
	}
	if err := os.MkdirAll(srcDir(store), 0o755); err != nil {
		return 0, err
	}
	for si, m := range st.meta.Members {
		if _, ok := readCkpt(store, m.Key); ok {
			continue
		}
		if err := restoreMember(store, st, si, m); err != nil {
			return restored, fmt.Errorf("part %s: %s: %w", id, m.Key, err)
		}
		restored++
	}
	return restored, nil
}

func parseRecords(members []member) error {
	if len(members) == 0 {
		return ErrNoParseRecord
	}
	for _, m := range members {
		if m.Harness == "" {
			return ErrNoParseRecord
		}
	}
	return nil
}

// fetchPart streams the blob to a temp file under parts/, checks it against
// sha256hex and unpacks it to parts/<id>.
func fetchPart(store, id, sha256hex string, fetch func(io.Writer) error) error {
	parts := filepath.Join(store, "parts")
	if err := os.MkdirAll(parts, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(parts, ".fetch-"+id)
	os.Remove(tmp)
	defer os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	err = fetch(io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha256hex {
		return fmt.Errorf("part %s: fetched blob sha256 %s, want %s", id, got, sha256hex)
	}
	return unpack(tmp, partDir(store, id))
}

// unpack writes the members of a blob Pack wrote into dir via dir.tmp and
// commitPart; the inverse of Pack, byte for byte.
func unpack(blobPath, dir string) error {
	blob, err := mmapFile(blobPath)
	if err != nil {
		return err
	}
	if len(blob) > 0 {
		defer syscall.Munmap(blob)
	}
	members, err := readPackIndex(blob)
	if err != nil {
		return fmt.Errorf("unpack %s: %w", blobPath, err)
	}
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	for name, b := range members {
		if err := os.WriteFile(filepath.Join(tmp, name), b, 0o644); err != nil {
			return err
		}
	}
	return commitPart(tmp, dir)
}

// restoreMember re-frames the rows of member si of st into generation 0 logs
// under store and then writes its checkpoint. The rows are the part's docs for
// that member, in doc order, with their heavy records placed in the new heavy
// log. The checkpoint comes last, by rename, so a crash before it leaves logs
// no checkpoint names.
func restoreMember(store string, st *Part, si int, m member) error {
	lightF, err := openLog(logPath(store, m.Key, 0, "light"), 0)
	if err != nil {
		return err
	}
	defer lightF.Close()
	heavyF, err := openLog(logPath(store, m.Key, 0, "heavy"), 0)
	if err != nil {
		return err
	}
	defer heavyF.Close()
	lw := &frameWriter{f: lightF, target: lightFrame, enc: encoder}
	hw := &frameWriter{f: heavyF, target: heavyFrame, enc: heavyEncoder}
	var frame uint64
	var recs [][]byte
	for d := 0; d < st.meta.Docs; d++ {
		if int(binary.LittleEndian.Uint16(st.docRec(d)[10:])) != si {
			continue
		}
		row, err := st.row(d)
		if err != nil {
			return err
		}
		own := *row
		if recs == nil || own.HeavyFrame != frame {
			if recs, _, err = readFrame(segments{st.heavyB, st.heavyD}, int64(own.HeavyFrame), nil); err != nil {
				return err
			}
			frame = own.HeavyFrame
		}
		if int(own.HeavyIdx) >= len(recs) {
			return fmt.Errorf("doc %d: heavy index past frame", d)
		}
		hf, hi, err := hw.add(recs[own.HeavyIdx])
		if err != nil {
			return err
		}
		own.HeavyFrame, own.HeavyIdx = uint64(hf), uint64(hi)
		if _, _, err := lw.add(own.append(nil)); err != nil {
			return err
		}
	}
	if err := hw.flush(); err != nil {
		return err
	}
	if err := lw.flush(); err != nil {
		return err
	}
	if err := lightF.Sync(); err != nil {
		return err
	}
	if err := heavyF.Sync(); err != nil {
		return err
	}
	ck := ckpt{parsed: m.parsed, LightLen: lw.off, HeavyLen: hw.off, ColdZst: m.ZstSize, Restored: true}
	b, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	return writeFileAtomic(ckptPath(store, m.Key), b)
}

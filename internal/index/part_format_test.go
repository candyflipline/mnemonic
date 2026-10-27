package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const (
	pinnedPartFormat = 7
	pinnedPartSHA    = "a56be3265d5f48cc2c557c288ffe057ef8e52bb4995c019bc671197eea9af652"
)

func TestPartBytesPinPartFormat(t *testing.T) {
	_, store := newQueryDB(t)
	cur, err := readManifest(store)
	if err != nil || len(cur.Parts) == 0 {
		t.Fatalf("parts %v, err %v", cur.Parts, err)
	}
	var sums []string
	for _, id := range cur.Parts {
		dir := partDir(store, id)
		var names []string
		if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(dir, p)
				names = append(names, rel)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		h := sha256.New()
		for _, n := range names {
			b, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(h, "%s %d\n", n, len(b))
			h.Write(b)
		}
		sums = append(sums, hex.EncodeToString(h.Sum(nil)))
	}
	sort.Strings(sums)
	h := sha256.New()
	for _, s := range sums {
		fmt.Fprintln(h, s)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got == pinnedPartSHA && partFormat == pinnedPartFormat {
		return
	}
	if partFormat == pinnedPartFormat {
		t.Fatalf("part bytes changed at partFormat %d: bump partFormat to %d, then set pinnedPartFormat and pinnedPartSHA to what this test reports next", partFormat, partFormat+1)
	}
	t.Fatalf("partFormat is %d: set pinnedPartFormat to %d and pinnedPartSHA to %s", partFormat, partFormat, got)
}

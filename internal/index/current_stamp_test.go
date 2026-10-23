package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentStampChangesWhenContentChangesInPlace(t *testing.T) {
	store := t.TempDir()
	cur := filepath.Join(store, "CURRENT")
	if err := os.WriteFile(cur, []byte(`{"parts":["a"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before, ok := CurrentStamp(store)
	if !ok {
		t.Fatal("no stamp")
	}
	if err := os.WriteFile(cur, []byte(`{"parts":["b"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := CurrentStamp(store)
	if before == after {
		t.Fatalf("stamp %d unchanged after CURRENT's bytes changed", before)
	}
}

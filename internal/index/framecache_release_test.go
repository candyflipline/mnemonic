package index

import (
	"runtime"
	"testing"
	"time"
	"weak"
)

func TestRetiredPartLeavesSharedFrameCache(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	retireInputs(t, root)
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	other := newDB([]*Part{{}}, nil)
	w := func() weak.Pointer[Part] {
		db, err := OpenDB(store)
		if err != nil {
			t.Fatal(err)
		}
		p := db.parts[0]
		other.fc = p.fc
		if _, err := p.row(0); err != nil {
			t.Fatal(err)
		}
		if len(p.fc.light) == 0 {
			t.Fatal("row(0) did not land in the frame cache")
		}
		db.Close()
		return weak.Make(p)
	}()
	runtime.GC()
	runtime.GC()
	if w.Value() != nil {
		t.Fatal("retired part still reachable")
	}
	runtime.KeepAlive(other)
}

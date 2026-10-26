package index

import (
	"encoding/json"
	"testing"
)

func TestDeltaImageHotReadsItsLastDoc(t *testing.T) {
	row := &Row{UID: "u1", Session: "s1", EventKind: "message", ActorKind: "user", Text: "alpha"}
	files := map[string][]byte{}
	write := func(name string, b []byte) error { files[name] = b; return nil }
	place := func(b built) (*Row, int64, int, error) { return b.row, 0, 0, nil }
	meta, _, err := indexRows(map[string][]built{"s1": {{row: row}}}, place, func(built) string { return "" }, write)
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(meta)
	st, err := openPart(mb, func(name string) ([]byte, error) { return files[name], nil })
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := st.hot(0)
	if err != nil {
		t.Fatal(err)
	}
	if r.UID != "u1" {
		t.Fatalf("hot(0).UID = %q, want u1", r.UID)
	}
}

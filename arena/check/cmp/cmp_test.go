package cmp

import "testing"

func TestDiffErrorStrings(t *testing.T) {
	got := Diff(Result{Error: "session not found"}, Result{Error: "reader unreachable"}, Mask{})
	if len(got) != 1 || got[0] != "error" {
		t.Fatalf("diff = %v, want [error]", got)
	}
	if d := Diff(Result{Error: "x"}, Result{Error: "x"}, Mask{}); len(d) != 0 {
		t.Fatalf("equal errors diff = %v", d)
	}
}

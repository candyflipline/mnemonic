package route

import (
	"slices"
	"testing"
)

func TestOwner(t *testing.T) {
	three := []string{"r1", "r2", "r3"}
	four := []string{"r1", "r2", "r3", "r4"}
	for _, c := range []struct {
		part         string
		want3, want4 []string
	}{
		{"p1", []string{"r2", "r3", "r1"}, []string{"r4", "r2", "r3", "r1"}},
		{"p2", []string{"r1", "r2", "r3"}, []string{"r1", "r4", "r2", "r3"}},
		{"p3", []string{"r3", "r1", "r2"}, []string{"r3", "r4", "r1", "r2"}},
		{"p4", []string{"r1", "r3", "r2"}, []string{"r1", "r3", "r2", "r4"}},
		{"p5", []string{"r1", "r2", "r3"}, []string{"r1", "r2", "r4", "r3"}},
		{"p6", []string{"r3", "r2", "r1"}, []string{"r3", "r2", "r1", "r4"}},
	} {
		if got := Owner(c.part, three); !slices.Equal(got, c.want3) {
			t.Errorf("Owner(%s, 3 readers) = %q, want %q", c.part, got, c.want3)
		}
		got := Owner(c.part, four)
		if !slices.Equal(got, c.want4) {
			t.Errorf("Owner(%s, 4 readers) = %q, want %q", c.part, got, c.want4)
		}
		if got[0] != c.want3[0] && got[0] != "r4" {
			t.Errorf("part %s moved from %s to %s, not to the new reader", c.part, c.want3[0], got[0])
		}
	}
}

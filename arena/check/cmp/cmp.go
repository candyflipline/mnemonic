// Package cmp compares an arena call result against an expected one: data and
// text, floats at a relative 1e-9, minus what the masks say varies between
// two runs of the oracle.
package cmp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

type Call struct {
	ID   int             `json:"id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

type Result struct {
	ID    int             `json:"id"`
	Data  json.RawMessage `json:"data"`
	Text  string          `json:"text"`
	Error string          `json:"error"`
	US    int64           `json:"us"`
}

// Mask is what one call may differ in between two runs of the oracle: data
// paths with indices, and text line numbers.
type Mask struct {
	Data      []string `json:"data,omitempty"`
	TextLines []int    `json:"text_lines,omitempty"`
}

// Masks is keyed by set name, then call ID.
type Masks map[string]map[int]Mask

func Decode(raw json.RawMessage, v *any) {
	if len(raw) == 0 {
		*v = nil
		return
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.Decode(v)
}

// Diff reports each place got differs from want, none when they are equal.
func Diff(want, got Result, mk Mask) []string {
	masked := make(map[string]bool, len(mk.Data))
	for _, x := range mk.Data {
		masked["data"+x] = true
	}
	var paths []string
	report := func(x string) { paths = append(paths, x) }
	var dw, dg any
	Decode(want.Data, &dw)
	Decode(got.Data, &dg)
	DiffPaths("data", dw, dg, masked, report)
	textDiff(want.Text, got.Text, mk.TextLines, report)
	if (want.Error == "") != (got.Error == "") {
		report("error")
	}
	return paths
}

// DiffPaths reports each path where a and b differ. A masked path matches
// anything; numbers match within a relative 1e-9 unless both are integers.
func DiffPaths(p string, a, b any, masked map[string]bool, report func(string)) {
	if masked[p] {
		return
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			report(p + ":type")
			return
		}
		keys := map[string]bool{}
		for k := range x {
			keys[k] = true
		}
		for k := range y {
			keys[k] = true
		}
		ks := make([]string, 0, len(keys))
		for k := range keys {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			va, oka := x[k]
			vb, okb := y[k]
			switch {
			case !oka:
				report(p + "." + k + ":extra")
			case !okb:
				report(p + "." + k + ":missing")
			default:
				DiffPaths(p+"."+k, va, vb, masked, report)
			}
		}
	case []any:
		y, ok := b.([]any)
		if !ok {
			report(p + ":type")
			return
		}
		if len(x) != len(y) {
			report(fmt.Sprintf("%s:len %d!=%d", p, len(x), len(y)))
		}
		for i := range min(len(x), len(y)) {
			DiffPaths(p+"["+strconv.Itoa(i)+"]", x[i], y[i], masked, report)
		}
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			report(p + ":type")
			return
		}
		if !numEqual(x, y) {
			report(p)
		}
	default:
		if a != b {
			report(p)
		}
	}
}

func numEqual(a, b json.Number) bool {
	if a == b {
		return true
	}
	ia, ea := a.Int64()
	ib, eb := b.Int64()
	if ea == nil && eb == nil {
		return ia == ib
	}
	fa, err1 := a.Float64()
	fb, err2 := b.Float64()
	if err1 != nil || err2 != nil {
		return false
	}
	if fa == fb {
		return true
	}
	return math.Abs(fa-fb) <= 1e-9*math.Max(math.Abs(fa), math.Abs(fb))
}

func textDiff(want, got string, maskedLines []int, report func(string)) {
	if want == got {
		return
	}
	lw, lg := strings.Split(want, "\n"), strings.Split(got, "\n")
	if len(lw) != len(lg) {
		report(fmt.Sprintf("text:lines %d!=%d", len(lw), len(lg)))
		return
	}
	skip := map[int]bool{}
	for _, i := range maskedLines {
		skip[i] = true
	}
	for i := range lw {
		if lw[i] != lg[i] && !skip[i] {
			report("text:line " + strconv.Itoa(i))
		}
	}
}

func ReadCalls(path string) ([]Call, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cs []Call
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, sc.Err()
}

func ReadResults(path string) (map[int]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rs := map[int]Result{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r Result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		rs[r.ID] = r
	}
	return rs, sc.Err()
}

func ReadMasks(path string) (Masks, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ms := Masks{}
	return ms, json.Unmarshal(b, &ms)
}

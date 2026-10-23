package ql

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

type Type uint8

const (
	TString Type = iota + 1
	TInt
	TFloat
	TBool
	TTime
	TStrings
	TIntMap
	TFloatMap
)

var typeNames = map[Type]string{
	TString: "string", TInt: "int", TFloat: "float", TBool: "bool", TTime: "time",
	TStrings: "list<string>", TIntMap: "map<string,int>", TFloatMap: "map<string,float>",
}

func (t Type) String() string { return typeNames[t] }

func (t Type) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

func (t Type) numeric() bool { return t == TInt || t == TFloat }

func (t Type) scalar() bool { return t >= TString && t <= TTime }

type Value struct {
	t Type
	n int64
	f float64
	s string
	x any
}

func Null() Value                   { return Value{} }
func StringValue(s string) Value    { return Value{t: TString, s: s} }
func IntValue(n int64) Value        { return Value{t: TInt, n: n} }
func FloatValue(f float64) Value    { return Value{t: TFloat, f: f} }
func TimeValue(t time.Time) Value   { return Value{t: TTime, n: t.UnixMicro()} }
func StringsValue(s []string) Value { return Value{t: TStrings, x: s} }

func BoolValue(b bool) Value {
	if b {
		return Value{t: TBool, n: 1}
	}
	return Value{t: TBool}
}

func IntMapValue(m map[string]int64) Value     { return Value{t: TIntMap, x: m} }
func FloatMapValue(m map[string]float64) Value { return Value{t: TFloatMap, x: m} }

func (v Value) Type() Type   { return v.t }
func (v Value) IsNull() bool { return v.t == 0 }

func (v Value) num() float64 {
	if v.t == TInt {
		return float64(v.n)
	}
	return v.f
}

func (v Value) truth() bool { return v.t == TBool && v.n == 1 }

func (v Value) time() time.Time { return time.UnixMicro(v.n).UTC() }

func (v Value) Any() any {
	switch v.t {
	case TString:
		return v.s
	case TInt:
		return v.n
	case TFloat:
		if math.IsNaN(v.f) || math.IsInf(v.f, 0) {
			return fmt.Sprint(v.f)
		}
		return v.f
	case TBool:
		return v.n == 1
	case TTime:
		return v.time().Format(time.RFC3339Nano)
	case TStrings, TIntMap, TFloatMap:
		return v.x
	}
	return nil
}

func (v Value) MarshalJSON() ([]byte, error) { return json.Marshal(v.Any()) }

func compare(a, b Value) int {
	if a.t.numeric() && b.t.numeric() {
		if a.t == TInt && b.t == TInt {
			return cmp3(a.n, b.n)
		}
		return cmp3(a.num(), b.num())
	}
	if a.t == TString {
		return strings.Compare(a.s, b.s)
	}
	return cmp3(a.n, b.n)
}

func cmp3[T int64 | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func appendKey(b []byte, v Value) []byte {
	switch v.t {
	case TString:
		b = append(b, byte(TString))
		b = binary.AppendUvarint(b, uint64(len(v.s)))
		return append(b, v.s...)
	case TInt:
		return binary.AppendVarint(append(b, byte(TInt)), v.n)
	case TFloat:
		if v.f == math.Trunc(v.f) && math.Abs(v.f) < 1<<63 {
			return binary.AppendVarint(append(b, byte(TInt)), int64(v.f))
		}
		return binary.LittleEndian.AppendUint64(append(b, byte(TFloat)), math.Float64bits(v.f))
	}
	return binary.AppendVarint(append(b, byte(v.t)), v.n)
}

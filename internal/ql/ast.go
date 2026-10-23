package ql

import "fmt"

type Pos struct{ Line, Col int }

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

type Query struct {
	From   Ident
	Shape  Shape
	Filter Expr
	Sort   []SortKey
	Limit  int
}

type Ident struct {
	At   Pos
	Name string
}

type Shape interface{ shape() }

type SelectAll struct{ At Pos }

type Select struct{ Items []Item }

type Group struct{ Dimensions, Measures []Item }

func (SelectAll) shape() {}
func (Select) shape()    {}
func (Group) shape()     {}

type Item struct {
	Expr Expr
	Name string
}

type SortKey struct {
	Expr Expr
	Desc bool
}

type Op string

const (
	OpOr  Op = "or"
	OpAnd Op = "and"
	OpNot Op = "not"
	OpNeg Op = "-"
	OpEq  Op = "="
	OpNe  Op = "!="
	OpLt  Op = "<"
	OpLe  Op = "<="
	OpGt  Op = ">"
	OpGe  Op = ">="
	OpAdd Op = "+"
	OpSub Op = "-"
	OpMul Op = "*"
	OpDiv Op = "/"
)

type Expr interface {
	Pos() Pos
	expr()
}

type Col struct {
	At   Pos
	Name string
}

type Lit struct {
	At  Pos
	Val Value
}

type Unary struct {
	At Pos
	Op Op
	X  Expr
}

type Binary struct {
	At   Pos
	Op   Op
	L, R Expr
}

type In struct {
	At   Pos
	X    Expr
	List []Lit
	Not  bool
}

type IsNull struct {
	At  Pos
	X   Expr
	Not bool
}

type Like struct {
	At      Pos
	X       Expr
	Pattern string
	Fold    bool
	Not     bool
}

type Match struct {
	At   Pos
	Col  Col
	Text string
}

type Includes struct {
	At   Pos
	X    Expr
	Elem string
}

type Index struct {
	At  Pos
	Col Col
	Key string
}

type Call struct {
	At   Pos
	Name string
	Args []Expr
	Star bool
}

func (e *Col) Pos() Pos      { return e.At }
func (e *Lit) Pos() Pos      { return e.At }
func (e *Unary) Pos() Pos    { return e.At }
func (e *Binary) Pos() Pos   { return e.At }
func (e *In) Pos() Pos       { return e.At }
func (e *IsNull) Pos() Pos   { return e.At }
func (e *Like) Pos() Pos     { return e.At }
func (e *Match) Pos() Pos    { return e.At }
func (e *Includes) Pos() Pos { return e.At }
func (e *Index) Pos() Pos    { return e.At }
func (e *Call) Pos() Pos     { return e.At }

func (*Col) expr()      {}
func (*Lit) expr()      {}
func (*Unary) expr()    {}
func (*Binary) expr()   {}
func (*In) expr()       {}
func (*IsNull) expr()   {}
func (*Like) expr()     {}
func (*Match) expr()    {}
func (*Includes) expr() {}
func (*Index) expr()    {}
func (*Call) expr()     {}

type Error struct {
	At  Pos
	Msg string
}

func (e *Error) Error() string {
	if e.At == (Pos{}) {
		return e.Msg
	}
	return e.At.String() + ": " + e.Msg
}

func errorf(at Pos, format string, args ...any) *Error {
	return &Error{At: at, Msg: fmt.Sprintf(format, args...)}
}

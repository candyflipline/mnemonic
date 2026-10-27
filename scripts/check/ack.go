package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

var ackMakers = []struct{ dir, pkg, maker string }{
	{"internal/store", "", "committed.acks"},
	{"internal/intake", "pushpb", "ackOf"},
}

func acks() ([]string, error) {
	var bad []string
	for _, m := range ackMakers {
		files, err := filepath.Glob(filepath.Join(m.dir, "*.go"))
		if err != nil {
			return nil, err
		}
		n, err := ackLiterals(files, m.pkg)
		if err != nil {
			return nil, err
		}
		if n == 1 {
			continue
		}
		lit := "Ack{"
		if m.pkg != "" {
			lit = m.pkg + "." + lit
		}
		bad = append(bad, fmt.Sprintf("%s holds %d `%s` composite literals, want exactly 1. Rule: an Ack is built only by %s; see docs/design/uploader.md. Route the new Ack through %s.", m.dir, n, lit, m.maker, m.maker))
	}
	return bad, nil
}

func ackLiterals(files []string, pkg string) (int, error) {
	fset := token.NewFileSet()
	n := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			return 0, err
		}
		ast.Inspect(f, func(x ast.Node) bool {
			if lit, ok := x.(*ast.CompositeLit); ok && isAck(lit.Type, pkg) {
				n++
			}
			return true
		})
	}
	return n, nil
}

func isAck(t ast.Expr, pkg string) bool {
	if pkg == "" {
		id, ok := t.(*ast.Ident)
		return ok && id.Name == "Ack"
	}
	sel, ok := t.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Ack" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}

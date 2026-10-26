package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

const (
	storeDir = "internal/store"
)

// acks fails unless internal/store builds an Ack in exactly one place.
func acks() ([]string, error) {
	files, err := filepath.Glob(filepath.Join(storeDir, "*.go"))
	if err != nil {
		return nil, err
	}
	n, err := ackLiterals(files)
	if err != nil {
		return nil, err
	}
	if n == 1 {
		return nil, nil
	}
	return []string{fmt.Sprintf("%s holds %d `Ack{` composite literals, want exactly 1. Rule: an Ack is built only by committed.acks; see docs/design/uploader.md. Route the new Ack through committed.acks.", storeDir, n)}, nil
}

func ackLiterals(files []string) (int, error) {
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
			if lit, ok := x.(*ast.CompositeLit); ok {
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "Ack" {
					n++
				}
			}
			return true
		})
	}
	return n, nil
}

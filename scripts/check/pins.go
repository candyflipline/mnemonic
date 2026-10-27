package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
)

const (
	oracleFile = "internal/redact/oracle_test.go"
	oracleSHA  = "46c437fd728cc8afbe5c2daa428ecf9f20d493c0424f52e823ea99bc3a0f74e2"

	toolsFile       = "internal/mcp/tools.json"
	faFile          = "internal/index/q_file_attention.go"
	faMaxResultsVar = "faMaxResults"
)

func oracleFrozen() ([]string, error) {
	b, err := os.ReadFile(oracleFile)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != oracleSHA {
		return []string{fmt.Sprintf("%s has sha256 %s, pinned %s. Rule: the fuzz oracle is frozen at main 59796bc so a redact.go change cannot move it (AGENTS.md, Rules, Frozen oracle). Revert the edit; a new oracle takes the owner's go, then set oracleSHA in scripts/check/pins.go.", oracleFile, got, oracleSHA)}, nil
	}
	return nil, nil
}

func fileAttentionMaxResults() ([]string, error) {
	raw, err := os.ReadFile(toolsFile)
	if err != nil {
		return nil, err
	}
	var tools []struct {
		Name        string
		InputSchema struct {
			Properties map[string]struct{ Maximum *int }
		}
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("%s: %w", toolsFile, err)
	}
	pinned := -1
	for _, t := range tools {
		if t.Name != "file_attention" {
			continue
		}
		if m := t.InputSchema.Properties["limit"].Maximum; m != nil {
			pinned = *m
		}
	}
	if pinned < 0 {
		return nil, fmt.Errorf("%s: file_attention has no limit maximum", toolsFile)
	}
	f, err := parser.ParseFile(token.NewFileSet(), faFile, nil, 0)
	if err != nil {
		return nil, err
	}
	got := -1
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if i >= len(vs.Values) {
				continue
			}
			if lit, ok := vs.Values[i].(*ast.BasicLit); ok && name.Name == faMaxResultsVar && lit.Kind == token.INT {
				got, _ = strconv.Atoi(lit.Value)
			}
		}
		return true
	})
	if got < 0 {
		return nil, fmt.Errorf("%s: no integer const %s", faFile, faMaxResultsVar)
	}
	if got != pinned {
		return []string{fmt.Sprintf("%s sets %s = %d, %s serves file_attention limit maximum %d. Rule: the tool's max_results bound is one number (AGENTS.md, Rules, Tool bounds). Set %s to %d; tools.json is served verbatim and only the owner changes it.", faFile, faMaxResultsVar, got, toolsFile, pinned, faMaxResultsVar, pinned)}, nil
	}
	return nil, nil
}

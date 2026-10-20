package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/candyflipline/mnemonic/internal/auth"
)

func token(args []string) error {
	if len(args) < 1 || args[0] != "new" {
		return fmt.Errorf("usage: mnemonic token new --author <name>")
	}
	author := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "--author" && i+1 < len(args) {
			i++
			author = args[i]
		}
	}
	if author == "" {
		return fmt.Errorf("--author <name> is required")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("read random bytes: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	fmt.Println(tok)
	fmt.Printf("[[tokens]]\nauthor = %q\nsha256 = %q\n", author, auth.Hash(tok))
	return nil
}

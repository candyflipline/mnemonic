// Package all registers every ported parser. Importers that parse (the
// server, the golden test) import it for its side effects.
package all

import (
	_ "github.com/candyflipline/mnemonic/internal/normalize/claudecode"
	_ "github.com/candyflipline/mnemonic/internal/normalize/codex"
	_ "github.com/candyflipline/mnemonic/internal/normalize/cursor"
	_ "github.com/candyflipline/mnemonic/internal/normalize/pi"
)

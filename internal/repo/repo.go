package repo

import (
	"encoding/json"

	"github.com/candyflipline/mnemonic/internal/wire"
)

type Book struct{ roots []wire.Root }

func DecodeBook(b []byte) (Book, error) {
	var roots []wire.Root
	if len(b) == 0 {
		return Book{}, nil
	}
	if err := json.Unmarshal(b, &roots); err != nil {
		return Book{}, err
	}
	return Book{roots: roots}, nil
}

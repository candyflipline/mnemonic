package export

import (
	"io"
	"time"

	"github.com/candyflipline/mnemonic/internal/index"
)

// LinkView is one read link as the renderers see it: the sessions the caller
// already checked are readable, in the owner's order, read as of Cutoff.
// MarkdownURL is the whole link's .md URL. SessionURL gives the URL of
// session n (1-based) in format ext, "md" or "json".
type LinkView struct {
	Title, Note string
	SessionIDs  []string
	Cutoff      time.Time
	MarkdownURL string
	SessionURL  func(n int, ext string) string
}

func WriteLinkHTML(w io.Writer, db *index.DB, v LinkView) error { panic("lane render") }

func WriteLinkMarkdown(w io.Writer, db *index.DB, v LinkView) error { panic("lane render") }

func WriteSessionMarkdown(w io.Writer, db *index.DB, v LinkView, n int) error {
	panic("lane render")
}

func WriteSessionATIF(w io.Writer, db *index.DB, v LinkView, n int) error { panic("lane render") }

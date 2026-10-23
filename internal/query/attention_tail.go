package query

import (
	"slices"
	"strings"

	"github.com/candyflipline/mnemonic/internal/repo"
)

type faTail struct {
	Rel        string
	Root       string
	ProjID     string
	Roots      []string
	Normalized bool
}

func faAbsolute(path string) (string, bool) {
	cleaned := strings.TrimPrefix(path, "./")
	normalized := repo.NormalizePath(cleaned)
	return normalized, strings.HasPrefix(normalized, "/") && faIsSinglePathCandidate(cleaned) && faIsSinglePathCandidate(normalized)
}

func faResolveTail(path string, seen []repo.Seen) faTail {
	cleaned := strings.TrimPrefix(path, "./")
	normalized := repo.NormalizePath(cleaned)
	normalizedChanged := normalized != path

	if abs, ok := faAbsolute(path); ok {
		if hit, ok := repo.Pick(abs, seen); ok {
			if rel, ok := faStripRoot(abs, hit.Root); ok && rel != "" {
				return faTail{Rel: rel, Root: hit.Root, ProjID: hit.ProjectID, Roots: faProjectRoots(seen, hit.ProjectID), Normalized: normalizedChanged}
			}
		}
	}

	return faTail{Rel: normalized, Normalized: normalizedChanged}
}

func faProjectRoots(seen []repo.Seen, projectID string) []string {
	var roots []string
	for _, c := range seen {
		if c.ProjectID == projectID {
			roots = append(roots, c.Root)
		}
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

func faIsSinglePathCandidate(path string) bool {
	if path == "" {
		return false
	}
	return !strings.ContainsAny(path, "\x00\n\r;|&`")
}

func faStripRoot(path, root string) (string, bool) {
	root = strings.TrimRight(root, "/")
	if root == "" {
		return "", false
	}
	rest, ok := strings.CutPrefix(path, root+"/")
	return rest, ok
}

func faTailSegments(rel string) int {
	n := 0
	for _, seg := range strings.Split(rel, "/") {
		if seg != "" {
			n++
		}
	}
	return n
}

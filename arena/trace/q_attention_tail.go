package trace

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type faTail struct {
	Rel        string
	Root       string
	ProjID     string
	Normalized bool
}

func faResolveTail(path string) faTail {
	cleaned := strings.TrimPrefix(path, "./")
	normalized := faNormalizePathText(cleaned)
	normalizedChanged := normalized != path

	if !strings.HasPrefix(normalized, "/") {
		return faTail{Rel: normalized, Normalized: normalizedChanged}
	}

	if faIsSinglePathCandidate(cleaned) && faIsSinglePathCandidate(normalized) {
		if root := qFindProjectRoot(normalized); root != "" {
			if rel, ok := faStripRoot(normalized, root); ok && rel != "" {
				return faTail{Rel: rel, Root: root, ProjID: qProjectIDForRoot(root), Normalized: normalizedChanged}
			}
		}
	}

	return faTail{Rel: normalized, Normalized: normalizedChanged}
}

func faIsSinglePathCandidate(path string) bool {
	if path == "" {
		return false
	}
	return !strings.ContainsAny(path, "\x00\n\r;|&`")
}

func faNormalizePathText(path string) string {
	absolute := strings.HasPrefix(path, "/")
	parts := make([]string, 0, len(path)/2+1)
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".":
		case "..":
			if n := len(parts); n > 0 && parts[n-1] != ".." {
				parts = parts[:n-1]
			} else if !absolute {
				parts = append(parts, part)
			}
		default:
			parts = append(parts, part)
		}
	}
	body := strings.Join(parts, "/")
	if absolute {
		if body == "" {
			return "/"
		}
		return "/" + body
	}
	return body
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

func qWorktreeRootsForRepoRoot(root string) []string {
	if root == "" {
		return nil
	}
	roots := []string{root}
	common, ok := qGitCommonDir(root)
	if !ok {
		return dedupSortedRoots(roots)
	}
	if parent := filepath.Dir(common); filepath.Base(common) == ".git" {
		roots = append(roots, parent)
	}
	worktreeGlob := common + "/worktrees/*/gitdir"
	matches, _ := filepath.Glob(worktreeGlob)
	for _, gitdirFile := range matches {
		b, err := os.ReadFile(gitdirFile)
		if err != nil {
			continue
		}
		gitDir := strings.TrimSpace(string(b))
		if worktree := filepath.Dir(gitDir); worktree != "" && worktree != "." {
			if worktreeCommon, ok := qGitCommonDir(worktree); ok && worktreeCommon == common {
				roots = append(roots, worktree)
			}
		}
	}
	return dedupSortedRoots(roots)
}

func dedupSortedRoots(roots []string) []string {
	clean := roots[:0:0]
	for _, r := range roots {
		if canon, ok := qCanonical(r); ok {
			clean = append(clean, canon)
		}
	}
	sort.Strings(clean)
	out := clean[:0]
	for i, r := range clean {
		if i == 0 || r != clean[i-1] {
			out = append(out, r)
		}
	}
	return out
}

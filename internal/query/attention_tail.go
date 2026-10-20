package query

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type faTail struct {
	Rel    string
	Root   string
	ProjID string
	// Abs records that the path was absolute and could not be reduced to a
	// project-relative tail, so Rel is matched literally.
	Abs        bool
	Normalized bool
	// DeriveRoots: stripping the tail from historical matched paths is safe
	// enough to report a worktree root.
	DeriveRoots bool
}

// faResolveTail ports resolve_tail_from; launchDir is empty on this server
// (no launch scope), but the parameter stays for the ported tests.
func faResolveTail(path, launchDir string) faTail {
	cleaned := strings.TrimPrefix(path, "./")
	rawSingle := faIsSinglePathCandidate(cleaned)
	normalized := faNormalizePathText(cleaned)
	normalizedChanged := normalized != path
	normalizedSingle := faIsSinglePathCandidate(normalized)
	root, projID := faLaunchProject(launchDir)

	if !strings.HasPrefix(normalized, "/") {
		if rawSingle && normalizedSingle && launchDir != "" && root != "" {
			absolute := faNormalizePathText(strings.TrimRight(launchDir, "/") + "/" + normalized)
			if rel, ok := faStripRoot(absolute, root); ok && rel != "" {
				return faTail{Rel: rel, Root: root, ProjID: projID, DeriveRoots: true, Normalized: normalizedChanged}
			}
		}
		return faTail{Rel: strings.TrimLeft(normalized, "/"), Normalized: normalizedChanged}
	}

	if rawSingle && normalizedSingle {
		targetRoot := qFindProjectRoot(normalized)
		if targetRoot == "" && root != "" {
			if _, ok := faStripRoot(normalized, root); ok {
				targetRoot = root
			}
		}
		if targetRoot != "" {
			if rel, ok := faStripRoot(normalized, targetRoot); ok && rel != "" {
				var projectID string
				switch {
				case launchDir == "":
					projectID = qProjectIDForRoot(targetRoot)
				case projID != "" && projID == qProjectIDForRoot(targetRoot):
					projectID = projID
				}
				return faTail{Rel: rel, Root: targetRoot, ProjID: projectID, DeriveRoots: true, Normalized: normalizedChanged}
			}
		}
	}

	return faTail{Rel: normalized, Abs: true, Normalized: normalizedChanged}
}

// faLaunchProject ports the launch_project binding: the Git boundary above the
// launch directory (probed through a routing marker so the launch dir itself
// counts), else the launch directory as a plain directory root.
func faLaunchProject(launchDir string) (string, string) {
	if launchDir == "" {
		return "", ""
	}
	root := qFindProjectRoot(filepath.Join(launchDir, ".mnemonic-project-scope"))
	if root == "" {
		if !qIsDir(launchDir) {
			return "", ""
		}
		root = launchDir
	}
	return root, qProjectIDForRoot(root)
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

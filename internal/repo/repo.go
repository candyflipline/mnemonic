// Package repo owns every rule about worktree roots and project ids. Its
// input is the root facts the session machine pushed; it never reads a disk.
package repo

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/candyflipline/mnemonic/internal/wire"
)

const maxBookEntries = 4096

var PathKeys = []string{"file_path", "notebook_path", "path", "target_file", "relativeWorkspacePath", "relative_workspace_path", "filepath", "file", "filename"}

var CwdKeys = []string{"cwd", "workspacePath"}

type Fields struct {
	ProjectID    string
	RepoRelPath  string
	WorktreeRoot string
}

type Book struct {
	roots []root
	facts []wire.Root
}

type root struct {
	path   string
	git    bool
	remote bool
	id     string
	from   uint64
}

func DecodeBook(b []byte) (Book, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return Book{}, nil
	}
	var facts []wire.Root
	if err := json.Unmarshal(b, &facts); err != nil {
		return Book{}, fmt.Errorf("repo: roots: %w", err)
	}
	if len(facts) > maxBookEntries {
		return Book{}, fmt.Errorf("repo: %d roots, at most %d", len(facts), maxBookEntries)
	}
	for _, f := range facts {
		if !strings.HasPrefix(f.Path, "/") || NormalizePath(f.Path) != f.Path {
			return Book{}, fmt.Errorf("repo: root path %q is not absolute and normalized", f.Path)
		}
		if f.Kind != wire.RootGit && f.Kind != wire.RootDir {
			return Book{}, fmt.Errorf("repo: root kind %q", f.Kind)
		}
		if slices.ContainsFunc([]string{f.Path, f.Key, f.Remote}, hasControl) {
			return Book{}, fmt.Errorf("repo: root fact carries a control character")
		}
		if splitRemote(f.Remote).userinfo {
			return Book{}, fmt.Errorf("repo: root remote carries userinfo")
		}
		if f.From < 0 {
			return Book{}, fmt.Errorf("repo: root from %d", f.From)
		}
	}
	facts = MergeRoots(nil, facts)
	book := Book{facts: facts, roots: make([]root, 0, len(facts))}
	for _, f := range facts {
		r := root{path: f.Path, git: f.Kind == wire.RootGit, from: uint64(f.From)}
		switch {
		case !r.git:
			r.id = dirID(f.Key)
		default:
			r.id, r.remote = RemoteID(f.Remote)
			if !r.remote {
				r.id = gitCommonDirID(f.Key)
			}
		}
		book.roots = append(book.roots, r)
	}
	slices.SortStableFunc(book.roots, func(a, b root) int {
		return cmp.Or(
			cmp.Compare(len(b.path), len(a.path)),
			cmp.Compare(a.path, b.path),
			boolFirst(a.git, b.git),
			boolFirst(a.remote, b.remote),
			cmp.Compare(a.from, b.from),
		)
	})
	return book, nil
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

func boolFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	default:
		return 1
	}
}

func (b Book) Event(cwd string, at uint64) Fields {
	cwd = NormalizePath(strings.TrimSpace(cwd))
	if !strings.HasPrefix(cwd, "/") {
		return Fields{}
	}
	for _, r := range b.roots {
		if _, in := relTo(cwd, r.path); r.git && r.from <= at && in {
			return Fields{ProjectID: r.id, WorktreeRoot: r.path}
		}
	}
	for _, r := range b.roots {
		if !r.git && r.from <= at && r.path == cwd {
			return Fields{ProjectID: r.id, WorktreeRoot: r.path}
		}
	}
	return Fields{}
}

func (b Book) Tool(cwd, path string, at uint64) (Fields, bool) {
	path = NormalizePath(strings.TrimSpace(path))
	cwd = NormalizePath(strings.TrimSpace(cwd))
	if path == "" || !strings.HasPrefix(cwd, "/") {
		return Fields{}, false
	}
	abs := path
	if !strings.HasPrefix(path, "/") {
		abs = NormalizePath(cwd + "/" + path)
	}
	for _, r := range b.roots {
		if !r.git || r.from > at {
			continue
		}
		rel, in := relTo(abs, r.path)
		switch {
		case !in:
		case rel == "":
			return Fields{}, false
		default:
			return Fields{ProjectID: r.id, RepoRelPath: rel, WorktreeRoot: r.path}, true
		}
	}
	for _, r := range b.roots {
		if r.git || r.from > at || r.path != cwd {
			continue
		}
		if rel, in := relTo(abs, r.path); in && rel != "" {
			return Fields{ProjectID: r.id, RepoRelPath: rel, WorktreeRoot: r.path}, true
		}
	}
	return Fields{}, false
}

func (b Book) Digest(before uint64) string {
	h := sha256.New()
	n := 0
	for _, f := range b.facts {
		if uint64(f.From) >= before {
			continue
		}
		n++
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\n", f.Path, f.Kind, f.Key, f.Remote, f.From)
	}
	if n == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

type rootKey struct {
	path   string
	kind   wire.RootKind
	key    string
	remote string
}

func MergeRoots(old, add []wire.Root) []wire.Root {
	from := map[rootKey]int64{}
	for _, f := range slices.Concat(old, add) {
		k := rootKey{f.Path, f.Kind, f.Key, f.Remote}
		if v, ok := from[k]; !ok || f.From < v {
			from[k] = f.From
		}
	}
	out := make([]wire.Root, 0, len(from))
	for k, v := range from {
		out = append(out, wire.Root{Path: k.path, Kind: k.kind, Key: k.key, Remote: k.remote, From: v})
	}
	slices.SortFunc(out, func(a, b wire.Root) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Remote, b.Remote))
	})
	return out
}

type Seen struct {
	Root, ProjectID string
}

func Pick(abs string, cands []Seen) (Seen, bool) {
	best := ""
	for _, c := range cands {
		if _, in := relTo(abs, c.Root); in && len(c.Root) > len(best) {
			best = c.Root
		}
	}
	if best == "" {
		return Seen{}, false
	}
	var hit Seen
	for _, c := range cands {
		switch {
		case c.Root != best:
		case hit.Root == "":
			hit = c
		case c.ProjectID != hit.ProjectID:
			return Seen{}, false
		}
	}
	return hit, true
}

func relTo(path, root string) (string, bool) {
	switch {
	case !strings.HasPrefix(root, "/"):
		return "", false
	case path == root:
		return "", true
	case root == "/" && strings.HasPrefix(path, "/"):
		return path[1:], true
	case strings.HasPrefix(path, root+"/"):
		return path[len(root)+1:], true
	default:
		return "", false
	}
}

func NormalizePath(path string) string {
	absolute := strings.HasPrefix(path, "/")
	var parts []string
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".":
		case "..":
			if len(parts) > 0 && parts[len(parts)-1] != ".." {
				parts = parts[:len(parts)-1]
				continue
			}
			if !absolute {
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

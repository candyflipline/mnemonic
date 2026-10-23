package push

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/candyflipline/mnemonic/internal/repo"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const maxProbeMemo = 4096

// prober is the only reader of a repo's disk. Its memo lives for one pass, so
// a repo created or removed between passes is seen.
type prober struct {
	red  Redactor
	home string
	git  map[string]*wire.Root
	dirs map[string]*wire.Root
}

func newProber(red Redactor) *prober {
	home, _ := os.UserHomeDir()
	p := &prober{red: red, home: home}
	p.reset()
	return p
}

func (p *prober) reset() {
	p.git = make(map[string]*wire.Root)
	p.dirs = make(map[string]*wire.Root)
}

func (p *prober) full() bool { return len(p.git)+len(p.dirs) >= maxProbeMemo }

// gitRoot is the innermost directory at or above dir that holds a .git, as a
// fact. A .git whose facts cannot be sent ends the walk with no fact, because
// an outer repo would misattribute the inner one's files.
func (p *prober) gitRoot(dir string) (wire.Root, bool) {
	if r, ok := p.git[dir]; ok {
		if r == nil {
			return wire.Root{}, false
		}
		return *r, true
	}
	var res *wire.Root
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if r, ok := p.gitFact(dir); ok {
			res = &r
		}
	} else if dir != p.home {
		if parent := filepath.Dir(dir); parent != dir {
			if r, ok := p.gitRoot(parent); ok {
				res = &r
			}
		}
	}
	if p.full() {
		p.reset()
	}
	p.git[dir] = res
	if res == nil {
		return wire.Root{}, false
	}
	return *res, true
}

func (p *prober) gitFact(root string) (wire.Root, bool) {
	common, ok := gitCommonDir(root)
	if !ok {
		return wire.Root{}, false
	}
	remote := repo.StripUserinfo(originURL(common))
	if !p.keeps(common) || !p.keeps(remote) {
		return wire.Root{}, false
	}
	return wire.Root{Path: root, Kind: wire.RootGit, Key: common, Remote: remote}, true
}

// dirRoot is the fact for a cwd outside any repo.
func (p *prober) dirRoot(cwd string) (wire.Root, bool) {
	if r, ok := p.dirs[cwd]; ok {
		if r == nil {
			return wire.Root{}, false
		}
		return *r, true
	}
	var res *wire.Root
	if isDir(cwd) {
		if canonical, err := filepath.EvalSymlinks(cwd); err == nil && p.keeps(canonical) {
			res = &wire.Root{Path: cwd, Kind: wire.RootDir, Key: canonical}
		}
	}
	if p.full() {
		p.reset()
	}
	p.dirs[cwd] = res
	if res == nil {
		return wire.Root{}, false
	}
	return *res, true
}

// keeps reports whether the redactor leaves s as it is; a fact with a string
// the redactor would change is dropped, never sent changed.
func (p *prober) keeps(s string) bool {
	return s == "" || bytes.Equal(p.red.Line([]byte(s)), []byte(s))
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func gitCommonDir(root string) (string, bool) {
	marker := filepath.Join(root, ".git")
	var gitDir string
	if isDir(marker) {
		gitDir = marker
	} else {
		content, err := os.ReadFile(marker)
		if err != nil {
			return "", false
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
		if !ok {
			return "", false
		}
		gitDir = joinFrom(root, strings.TrimSpace(rest))
	}
	commonDir := gitDir
	if content, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		commonDir = joinFrom(gitDir, strings.TrimSpace(string(content)))
	}
	canonical, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", false
	}
	return canonical, true
}

func joinFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// originURL reads remote "origin" from <common dir>/config.
func originURL(commonDir string) string {
	f, err := os.Open(filepath.Join(commonDir, "config"))
	if err != nil {
		return ""
	}
	defer f.Close()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			in = isOriginHeader(line)
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.EqualFold(strings.TrimSpace(k), "url") {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

func isOriginHeader(line string) bool {
	head, rest, ok := strings.Cut(strings.TrimPrefix(line, "["), "\"")
	if !ok || !strings.EqualFold(strings.TrimSpace(head), "remote") {
		return false
	}
	name, _, ok := strings.Cut(rest, "\"")
	return ok && name == "origin"
}

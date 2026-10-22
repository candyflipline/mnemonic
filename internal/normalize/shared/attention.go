package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

var fileAttentionPathKeys = map[string]bool{
	"file_path": true, "notebook_path": true, "path": true, "target_file": true,
	"relativeWorkspacePath": true, "relative_workspace_path": true, "filepath": true,
	"file": true, "filename": true,
}

type FileAttentionFields struct {
	ProjectID    string
	RepoRelPath  string
	WorktreeRoot string
}

// Probes remembers filesystem answers for the length of one parse, so a
// source's repeated cwds and roots are probed once. A nil Probes probes every
// time. Each map stops growing at probeLimit entries.
type Probes struct {
	event   map[string]FileAttentionFields
	root    map[string]rootHit
	dir     map[string]bool
	git     map[string]bool
	project map[string]rootHit
}

type rootHit struct {
	root string
	ok   bool
}

const probeLimit = 4096

func NewProbes() *Probes {
	return &Probes{event: map[string]FileAttentionFields{}, root: map[string]rootHit{},
		dir: map[string]bool{}, git: map[string]bool{}, project: map[string]rootHit{}}
}

func remember[V any](m map[string]V, k string, f func() V) V {
	if v, ok := m[k]; ok {
		return v
	}
	v := f()
	if len(m) < probeLimit {
		m[k] = v
	}
	return v
}

func (p *Probes) eventFields(cwd string) FileAttentionFields {
	if p == nil {
		return eventFileAttentionFields(p, cwd)
	}
	return remember(p.event, cwd, func() FileAttentionFields { return eventFileAttentionFields(p, cwd) })
}

func (p *Probes) findRoot(path string) (string, bool) {
	if p == nil {
		return findFileAttentionRoot(p, path)
	}
	h := remember(p.root, path, func() rootHit { r, ok := findFileAttentionRoot(p, path); return rootHit{r, ok} })
	return h.root, h.ok
}

func (p *Probes) isDir(path string) bool {
	if p == nil {
		return isDir(path)
	}
	return remember(p.dir, path, func() bool { return isDir(path) })
}

func (p *Probes) hasGit(dir string) bool {
	probe := func() bool { _, err := os.Stat(filepath.Join(dir, ".git")); return err == nil }
	if p == nil {
		return probe()
	}
	return remember(p.git, dir, probe)
}

func (p *Probes) projectID(root string) (string, bool) {
	if p == nil {
		return ProjectIDForRoot(root)
	}
	h := remember(p.project, root, func() rootHit { r, ok := ProjectIDForRoot(root); return rootHit{r, ok} })
	return h.root, h.ok
}

func EventFileAttentionFields(p *Probes, cwd string) FileAttentionFields {
	return p.eventFields(cwd)
}

func eventFileAttentionFields(p *Probes, cwd string) FileAttentionFields {
	cwd = NormalizePathText(strings.TrimSpace(cwd))
	if cwd == "" || !strings.HasPrefix(cwd, "/") {
		return FileAttentionFields{}
	}
	root, ok := p.findRoot(cwd)
	if !ok {
		if p.isDir(cwd) {
			root, ok = cwd, true
		}
	}
	if !ok {
		return FileAttentionFields{}
	}
	projectID, ok := p.projectID(root)
	if !ok {
		return FileAttentionFields{}
	}
	return FileAttentionFields{ProjectID: projectID, WorktreeRoot: root}
}

func ToolFileAttentionFields(p *Probes, cwd, toolPhase, inputJSON string) FileAttentionFields {
	if toolPhase != "request" {
		return FileAttentionFields{}
	}
	input, err := DecodeJSON([]byte(inputJSON))
	if err != nil {
		return FileAttentionFields{}
	}
	var candidates []string
	collectStructuredPathCandidates(input, &candidates)
	if len(candidates) != 1 {
		return FileAttentionFields{}
	}
	fields, ok := resolveToolPathFields(p, cwd, candidates[0])
	if !ok {
		return FileAttentionFields{}
	}
	return fields
}

func resolveToolPathFields(p *Probes, cwd, rawPath string) (FileAttentionFields, bool) {
	rawPath = strings.TrimSpace(rawPath)
	if !isSinglePathCandidate(rawPath) {
		return FileAttentionFields{}, false
	}
	normalized := NormalizePathText(rawPath)
	if normalized == "" {
		return FileAttentionFields{}, false
	}
	cwd = NormalizePathText(strings.TrimSpace(cwd))
	if cwd == "" || !strings.HasPrefix(cwd, "/") {
		return FileAttentionFields{}, false
	}
	absolute := normalized
	if !strings.HasPrefix(normalized, "/") {
		absolute = NormalizePathText(strings.TrimSuffix(cwd, "/") + "/" + normalized)
	}
	root, ok := p.findRoot(absolute)
	if !ok && p.isDir(cwd) && stripRoot(absolute, cwd) != "" {
		root, ok = cwd, true
	}
	if !ok {
		return FileAttentionFields{}, false
	}
	projectID, ok := p.projectID(root)
	if !ok {
		return FileAttentionFields{}, false
	}
	repoRelPath := stripRoot(absolute, root)
	if repoRelPath == "" {
		return FileAttentionFields{}, false
	}
	return FileAttentionFields{ProjectID: projectID, RepoRelPath: repoRelPath, WorktreeRoot: root}, true
}

func collectStructuredPathCandidates(value JSON, out *[]string) {
	switch t := value.(type) {
	case map[string]any:
		for key, item := range t {
			if fileAttentionPathKeys[key] {
				collectPathValues(item, out)
			}
			collectStructuredPathCandidates(item, out)
		}
	case []any:
		for _, item := range t {
			collectStructuredPathCandidates(item, out)
		}
	}
}

func collectPathValues(value JSON, out *[]string) {
	switch t := value.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			*out = append(*out, t)
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				*out = append(*out, s)
			}
		}
	}
}

func isSinglePathCandidate(path string) bool {
	if path == "" || strings.ContainsRune(path, 0) {
		return false
	}
	return !strings.ContainsAny(path, "\n\r;|&`")
}

func NormalizePathText(path string) string {
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

func findFileAttentionRoot(p *Probes, path string) (string, bool) {
	home := os.Getenv("HOME")
	dir := path
	if !p.isDir(dir) {
		dir = filepath.Dir(dir)
	}
	for {
		if p.hasGit(dir) {
			return dir, true
		}
		if home != "" && dir == home {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func stripRoot(path, root string) string {
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		return ""
	}
	return strings.TrimPrefix(path, root+"/")
}

// Git repositories key on their common dir, non-git directories on their
// canonical path.
func ProjectIDForRoot(root string) (string, bool) {
	if commonDir, ok := gitCommonDir(root); ok {
		return "git:" + hashPathDomain("moraine-git-common-dir\x00", commonDir), true
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	if !isDir(canonical) {
		return "", false
	}
	return "dir:" + hashPathDomain("moraine-directory-root\x00", canonical), true
}

func hashPathDomain(domain, path string) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte(path))
	return hex.EncodeToString(h.Sum(nil))
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
		text := strings.TrimSpace(string(content))
		rest, ok := strings.CutPrefix(text, "gitdir:")
		if !ok {
			return "", false
		}
		p := strings.TrimSpace(rest)
		if filepath.IsAbs(p) {
			gitDir = p
		} else {
			gitDir = filepath.Join(root, p)
		}
	}
	commonDir := gitDir
	commonDirFile := filepath.Join(gitDir, "commondir")
	if isFile(commonDirFile) {
		content, err := os.ReadFile(commonDirFile)
		if err == nil {
			p := strings.TrimSpace(string(content))
			if filepath.IsAbs(p) {
				commonDir = p
			} else {
				commonDir = filepath.Join(gitDir, p)
			}
		}
	}
	canonical, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", false
	}
	return canonical, true
}

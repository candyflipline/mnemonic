package push

import (
	"bytes"
	"cmp"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/candyflipline/mnemonic/internal/repo"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const maxSniffedValue = 4096

var (
	plainValue   = regexp.MustCompile(`"(` + keyAlternation() + `)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	escapedValue = regexp.MustCompile(`\\"(` + keyAlternation() + `)\\"\s*:\s*\\"((?:[^"\\]|\\[^"])*)\\"`)
)

func keyAlternation() string {
	var keys []string
	for _, k := range slices.Concat(repo.CwdKeys, repo.PathKeys) {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	return strings.Join(keys, "|")
}

type sniffed struct {
	value string
	cwd   bool
}

// lineValues are the values of repo.CwdKeys and repo.PathKeys in one
// redacted line, as plain JSON or one level escaped.
func lineValues(line []byte) []sniffed {
	var out []sniffed
	for _, m := range plainValue.FindAllSubmatch(line, -1) {
		if v, ok := decodeValue(m[2], 1); ok {
			out = append(out, sniffed{v, slices.Contains(repo.CwdKeys, string(m[1]))})
		}
	}
	for _, m := range escapedValue.FindAllSubmatch(line, -1) {
		if v, ok := decodeValue(m[2], 2); ok {
			out = append(out, sniffed{v, slices.Contains(repo.CwdKeys, string(m[1]))})
		}
	}
	return out
}

func decodeValue(raw []byte, levels int) (string, bool) {
	s := string(raw)
	for range levels {
		if err := json.Unmarshal([]byte(`"`+s+`"`), &s); err != nil {
			return "", false
		}
	}
	if s == "" || len(s) > maxSniffedValue || strings.ContainsAny(s, "\x00\n") {
		return "", false
	}
	return s, true
}

type factKey struct {
	path, key, remote string
	kind              wire.RootKind
}

// sniff returns the root facts for the lines of body, whose first line starts
// at redacted offset base. Each fact's Path is an ancestor of a path its own
// line names, and its From is the first line that named it.
func sniff(body []byte, base int64, p *prober) []wire.Root {
	var out []wire.Root
	have := make(map[factKey]bool)
	probed := make(map[sniffed]bool)
	add := func(r wire.Root, ok bool, at int64) {
		k := factKey{r.Path, r.Key, r.Remote, r.Kind}
		if !ok || have[k] {
			return
		}
		have[k] = true
		r.From = at
		if repo.ValidRoot(r) != nil {
			return
		}
		out = append(out, r)
	}
	for at := base; len(body) > 0; {
		end := len(body)
		if nl := bytes.IndexByte(body, '\n'); nl >= 0 {
			end = nl + 1
		}
		line := body[:end]
		body = body[end:]
		vals := lineValues(line)
		cwd := ""
		for _, v := range vals {
			if v.cwd && strings.HasPrefix(v.value, "/") {
				cwd = repo.NormalizePath(v.value)
				break
			}
		}
		for _, v := range vals {
			path := v.value
			if !strings.HasPrefix(path, "/") {
				if cwd == "" {
					continue
				}
				path = cwd + "/" + path
			}
			path = repo.NormalizePath(path)
			if probed[sniffed{path, v.cwd}] {
				continue
			}
			probed[sniffed{path, v.cwd}] = true
			start := path
			if !isDir(start) {
				start = filepath.Dir(start)
			}
			r, ok := p.gitRoot(start)
			if !ok && v.cwd {
				r, ok = p.dirRoot(path)
			}
			add(r, ok, at)
		}
		at += int64(end)
	}
	slices.SortStableFunc(out, func(a, b wire.Root) int { return cmp.Compare(a.From, b.From) })
	return out
}

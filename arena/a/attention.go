package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
)

type faQuery struct {
	Rel             string
	NormalizedPID   string
	NormalizedRoots []string
	DeriveLegacy    bool
	ProjectScope    bool
	StartUnixMs     *int64
	EndUnixMs       *int64
	Tool            string
	Harness         string
	SourceName      string
	MutationsOnly   bool
	MaxRows         int
}

type faTouch struct {
	SessionID     string
	EventUID      string
	ToolCallID    string
	Harness       string
	SourceName    string
	ToolName      string
	ToolPhase     string
	MatchedPath   string
	MatchKind     string
	WorktreeRoot  string
	CWD           string
	EventUnixMs   *int64
	EventOrder    uint64
	TurnSeq       *uint32
	InputPreview  string
	OutputPreview string
}

var faReadTools = []string{"read", "readfile", "read_file", "notebookread", "notebook_read", "view", "cat", "grep", "rg", "glob", "ls", "list", "find"}

var faPathKeys = []string{
	"file_path", "notebook_path", "path", "target_file", "relativeWorkspacePath",
	"relative_workspace_path", "filepath", "file", "filename",
}

var faLiteralRun = regexp.MustCompile(`[A-Za-z0-9._-]+`)

func faSingle(s string) bool {
	return s != "" && !strings.ContainsAny(s, "\x00\n\r;|&`")
}

func faFilterRow(r *Row, q faQuery) bool {
	if r.ToolPhase != "request" {
		return false
	}
	tn := strings.ToLower(r.ToolName)
	if tn == "file_attention" || strings.HasSuffix(tn, "_file_attention") {
		return false
	}
	if q.Tool != "" && tn != strings.ToLower(q.Tool) {
		return false
	}
	if q.Harness != "" && r.Harness != q.Harness {
		return false
	}
	if q.MutationsOnly && slices.Contains(faReadTools, tn) {
		return false
	}
	return true
}

func faTimeOK(ms int64, sourceName string, q faQuery) bool {
	if q.StartUnixMs != nil && ms < *q.StartUnixMs {
		return false
	}
	if q.EndUnixMs != nil && ms >= *q.EndUnixMs {
		return false
	}
	if q.SourceName != "" && sourceName != q.SourceName {
		return false
	}
	return true
}

func (st *Store) touchOf(d int, r *Row) faTouch {
	ms := timeMs(r)
	seq := st.turnSeq(d)
	s := st.session(st.sessionOf(d))
	return faTouch{
		SessionID: r.Session, EventUID: r.UID, ToolCallID: r.ToolCallID, Harness: r.Harness,
		SourceName: r.SourceName, ToolName: r.ToolName, ToolPhase: r.ToolPhase, CWD: r.Cwd,
		EventUnixMs: &ms, EventOrder: uint64(d - s.start + 1), TurnSeq: &seq,
		InputPreview: r.TioInPrev, OutputPreview: r.TioOutPrev,
	}
}

func (st *Store) faTouches(q faQuery) ([]faTouch, error) {
	if q.Rel == "" {
		return nil, argErrorf("file_attention requires a non-empty path tail")
	}
	if q.ProjectScope && q.NormalizedPID == "" {
		return []faTouch{}, nil
	}
	limitPlus := q.MaxRows + 1
	exact, err := st.faExactTouches(q)
	if err != nil {
		return nil, err
	}
	suffix, err := st.faSuffixTouches(q)
	if err != nil {
		return nil, err
	}
	sortTouches(exact)
	if len(exact) > limitPlus {
		exact = exact[:limitPlus]
	}
	sortTouches(suffix)
	if len(suffix) > limitPlus {
		suffix = suffix[:limitPlus]
	}
	return faMergeTouches(append(exact, suffix...), limitPlus), nil
}

func sortTouches(t []faTouch) {
	sort.SliceStable(t, func(i, j int) bool { return faTouchLess(&t[i], &t[j]) })
}

func (st *Store) faExactTouches(q faQuery) ([]faTouch, error) {
	var out []faTouch
	b := st.faExact
	for len(b) > 0 {
		l, k := binary.Uvarint(b)
		rel := string(b[k : k+int(l)])
		d := int(binary.LittleEndian.Uint32(b[k+int(l):]))
		b = b[k+int(l)+4:]
		if rel != q.Rel {
			if rel > q.Rel {
				break
			}
			continue
		}
		r, err := st.row(d)
		if err != nil {
			return nil, err
		}
		if !faFilterRow(r, q) {
			continue
		}
		if q.ProjectScope {
			if r.ProjectID != q.NormalizedPID {
				continue
			}
		} else if r.ProjectID == "" {
			continue
		}
		if !faTimeOK(timeMs(r), r.SourceName, q) {
			continue
		}
		t := st.touchOf(d, r)
		t.MatchKind = "path_suffix"
		t.MatchedPath = q.Rel
		if faSingle(r.WorktreeRoot) {
			t.MatchedPath = r.WorktreeRoot + "/" + q.Rel
			t.WorktreeRoot = r.WorktreeRoot
		}
		out = append(out, t)
	}
	return out, nil
}

type faRegexps struct {
	scalar, array, shell, provenance *regexp.Regexp
}

func faCompile(rel string) faRegexps {
	keyRe := strings.Join(faPathKeys, "|")
	relRe := regexp.QuoteMeta(rel)
	slashRelRe := regexp.QuoteMeta("/" + rel)
	return faRegexps{
		scalar:     regexp.MustCompile("\"(?:" + keyRe + ")\"[[:space:]]*:[[:space:]]*\"((?:[^\"\\\\]|\\\\.)*" + slashRelRe + "|" + relRe + ")\""),
		array:      regexp.MustCompile("\"(?:" + keyRe + ")\"[[:space:]]*:[[:space:]]*\\[[^\\]]*\"((?:[^\"\\\\]|\\\\.)*" + slashRelRe + "|" + relRe + ")\""),
		shell:      regexp.MustCompile("(^|[[:space:]'\"`=(])(/[^/][^[:space:]'\"`<>|;&),]*" + slashRelRe + "|/" + relRe + "|" + relRe + ")([[:space:]'\"`,;|&<>)]|$)"),
		provenance: regexp.MustCompile("\"(?:" + keyRe + "|command|cmd)\"[[:space:]]*:"),
	}
}

func regexpExtract1(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}

// jsonFields reads the top-level keys of a JSON object the way
// json_extract_string(input, '$.key') sees them; ok is json_valid.
func jsonFields(input string) (map[string]json.RawMessage, bool) {
	if !json.Valid([]byte(input)) {
		return nil, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &m) != nil {
		return map[string]json.RawMessage{}, true
	}
	return m, true
}

func jstr(m map[string]json.RawMessage, key string) string {
	v, ok := m[key]
	if !ok || string(v) == "null" {
		return ""
	}
	return jsonExtractString(v)
}

func (st *Store) faSuffixTouches(q faQuery) ([]faTouch, error) {
	rel := q.Rel
	runs := []string{}
	for _, run := range faLiteralRun.FindAllString(rel, -1) {
		if !slices.Contains(runs, run) {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return len(runs[i]) > len(runs[j]) })
	needles := make([][]byte, len(runs))
	for i, r := range runs {
		needles[i] = []byte(r)
	}
	var res *faRegexps
	var roots []string
	if q.ProjectScope {
		for _, r := range q.NormalizedRoots {
			if r != "" {
				roots = append(roots, r)
			}
		}
		sort.SliceStable(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
		roots = dedupAdjacent(roots)
	}
	var out []faTouch
	n := len(st.faTio)/8 - 1
	for i := 0; i < n; i++ {
		e := st.faTio[i*8:]
		lo := binary.LittleEndian.Uint32(e[4:])
		hi := binary.LittleEndian.Uint32(e[12:])
		blob := st.faBlob[lo:hi]
		ok := true
		for _, nd := range needles {
			if !bytes.Contains(blob, nd) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		d := int(binary.LittleEndian.Uint32(e))
		r, err := st.row(d)
		if err != nil {
			return nil, err
		}
		if !faFilterRow(r, q) {
			continue
		}
		if res == nil {
			c := faCompile(rel)
			res = &c
		}
		in := r.TioInput
		fields, valid := jsonFields(in)
		var scalars [9]string
		keyMatch := false
		legacyFirst := ""
		nonEmpty := 0
		for k, key := range faPathKeys {
			v := ""
			if valid {
				v = jstr(fields, key)
			}
			scalars[k] = v
			if v != "" {
				nonEmpty++
			}
			if strings.HasSuffix(v, "/"+rel) || v == rel {
				keyMatch = true
				if legacyFirst == "" {
					legacyFirst = v
				}
			}
		}
		scalarX := regexpExtract1(res.scalar, in)
		arrayX := regexpExtract1(res.array, in)
		nested := scalarX != "" || arrayX != ""
		shell := false
		if valid {
			cmd := jstr(fields, "command")
			if cmd == "" {
				cmd = jstr(fields, "cmd")
			}
			shell = res.shell.MatchString(cmd)
		}
		if !keyMatch && !nested && !shell {
			continue
		}
		nestedPath := scalarX
		if nestedPath == "" {
			nestedPath = arrayX
		}
		matched := legacyFirst
		if matched == "" {
			matched = nestedPath
		}
		candidate := ""
		if q.DeriveLegacy && !strings.HasPrefix(rel, "/") {
			if len(res.provenance.FindAllStringIndex(in, -1)) == 1 && nonEmpty == 1 && faSingle(legacyFirst) && faSingle(r.Cwd) &&
				(legacyFirst == rel || legacyFirst == r.Cwd+"/"+rel) {
				candidate = r.Cwd
			}
		}
		legacyRoot := candidate
		if q.ProjectScope {
			verified := ""
			for _, root := range roots {
				if candidate == root {
					verified = root
					break
				}
			}
			toolScope := r.TioProj != "" && !strings.HasPrefix(r.TioProj, "git:") && faSingle(r.TioWT) && slices.Contains(roots, r.TioWT)
			eventScope := r.ProjectID != "" && !strings.HasPrefix(r.ProjectID, "git:") && faSingle(r.WorktreeRoot) && slices.Contains(roots, r.WorktreeRoot)
			if !(r.TioProj == q.NormalizedPID || toolScope ||
				(r.TioProj == "" && r.ProjectID == q.NormalizedPID) ||
				(r.TioProj == "" && candidate != "" && (eventScope || (r.ProjectID == "" && verified != "")))) {
				continue
			}
			legacyRoot = verified
		}
		if !faTimeOK(timeMs(r), r.SourceName, q) {
			continue
		}
		t := st.touchOf(d, r)
		t.MatchedPath = matched
		t.MatchKind = "shell_path"
		if matched != "" {
			t.MatchKind = "path_suffix"
		}
		switch {
		case r.TioProj != "" && r.TioRel == rel && faSingle(r.TioWT):
			t.WorktreeRoot = r.TioWT
		case candidate != "":
			t.WorktreeRoot = legacyRoot
		}
		out = append(out, t)
	}
	return out, nil
}

func faMergeTouches(touches []faTouch, limit int) []faTouch {
	seen := make(map[string]bool, len(touches))
	merged := make([]faTouch, 0, len(touches))
	for _, t := range touches {
		key := t.SessionID + "\x00" + t.ToolCallID + "\x00" + t.EventUID
		if !seen[key] {
			seen[key] = true
			merged = append(merged, t)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return faTouchLess(&merged[i], &merged[j])
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func faTouchLess(a, b *faTouch) bool {
	switch {
	case a.EventUnixMs != nil && b.EventUnixMs != nil:
		if *a.EventUnixMs != *b.EventUnixMs {
			return *a.EventUnixMs > *b.EventUnixMs
		}
	case a.EventUnixMs != nil:
		return true
	case b.EventUnixMs != nil:
		return false
	}
	if a.EventOrder != b.EventOrder {
		return a.EventOrder > b.EventOrder
	}
	return a.EventUID > b.EventUID
}

func dedupAdjacent(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

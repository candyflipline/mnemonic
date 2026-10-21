package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

type pathIndex struct {
	paths                   []pathEntry
	pathFallback, shellDocs []uint32
}

type pathEntry struct {
	reversed string
	docs     []uint32
}

var pathScalar = regexp.MustCompile(`"(?:` + strings.Join(faPathKeys, "|") + `)"[[:space:]]*:[[:space:]]*"((?:[^"\\]|\\.)*)"`)
var pathArrayStart = regexp.MustCompile(`"(?:` + strings.Join(faPathKeys, "|") + `)"[[:space:]]*:[[:space:]]*\[`)
var pathArray = regexp.MustCompile(`"(?:` + strings.Join(faPathKeys, "|") + `)"[[:space:]]*:[[:space:]]*\[([^\]]*)\]`)
var pathQuoted = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

func reverseBytes(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
func jsonStringField(obj map[string]json.RawMessage, k string) string {
	r := obj[k]
	if len(r) == 0 || string(r) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return string(r)
}
func (db *Store) buildPaths() {
	if len(db.segments) == 0 {
		db.buildOnePathIndex()
		return
	}
	for _, sg := range db.segments {
		tmp := Store{events: sg.pending}
		tmp.buildOnePathIndex()
		freezePaths(&db.arena, &tmp.pathIndex)
		sg.pathIndex = tmp.pathIndex
		sg.pending = nil
	}
}
func (db *Store) buildOnePathIndex() {
	paths := map[string][]uint32{}
	for doc, e := range db.events {
		if e.ToolPhase != "request" {
			continue
		}
		seen := map[string]bool{}
		add := func(p string) {
			if p != "" && !seen[p] {
				seen[p] = true
				paths[p] = append(paths[p], uint32(doc))
			}
		}
		if e.ProjectID != "" {
			add(e.RepoRelPath)
		}
		io := e.IO
		if io == nil {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(io.Input), &obj); err != nil {
			// The SQL regex branch accepts malformed input. Preserve those rare rows
			// as a conservative fallback rather than guessing a parse tree.
			db.pathFallback = append(db.pathFallback, uint32(doc))
			continue
		}
		for _, k := range faPathKeys {
			add(jsonStringField(obj, k))
		}
		// Arrays can contain a literal ] inside a string. The SQL's raw regex
		// has unusual capture semantics there, so keep array rows as fallback.
		if pathArrayStart.MatchString(io.Input) {
			db.pathFallback = append(db.pathFallback, uint32(doc))
		}
		// SQL matches raw nested captures, including their JSON escaping.
		for _, m := range pathScalar.FindAllStringSubmatch(io.Input, -1) {
			add(m[1])
		}
		for _, m := range pathArray.FindAllStringSubmatch(io.Input, -1) {
			for _, v := range pathQuoted.FindAllStringSubmatch(m[1], -1) {
				add(v[1])
			}
		}
		cmd := firstNonEmpty(jsonStringField(obj, "command"), jsonStringField(obj, "cmd"))
		if cmd != "" {
			db.shellDocs = append(db.shellDocs, uint32(doc))
		}
		// Preserve words and path tokens at every legal left boundary. The final
		// exact SQL-equivalent regex still checks every candidate at query time.
		for _, token := range strings.FieldsFunc(cmd, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '\v' || r == '\f' || strings.ContainsRune("'\"`,;|&<>)]", r)
		}) {
			add(token)
			for i, c := range token {
				if c == '=' || c == '(' || c == '`' {
					add(token[i+1:])
				}
			}
		}
	}
	db.paths = make([]pathEntry, 0, len(paths))
	for p, docs := range paths {
		db.paths = append(db.paths, pathEntry{reverseBytes(p), docs})
	}
	sort.Slice(db.paths, func(i, j int) bool { return db.paths[i].reversed < db.paths[j].reversed })
}
func (db *Store) attentionCandidates(rel string) []*event {
	if len(db.segments) == 0 && db.paths == nil {
		return db.events
	}
	rev := reverseBytes(rel)
	seen := map[uint32]bool{}
	out := make([]*event, 0)
	add := func(doc uint32) {
		if doc != ^uint32(0) && !seen[doc] {
			seen[doc] = true
			out = append(out, db.events[doc])
		}
	}
	scan := func(p *pathIndex, mapping []uint32) {
		emit := func(doc uint32) {
			if mapping != nil {
				doc = mapping[doc]
			}
			add(doc)
		}
		if strings.ContainsAny(rel, " \t\r\n\v\f'\"`,;|&<>)]") {
			for _, doc := range p.shellDocs {
				emit(doc)
			}
		}
		for _, doc := range p.pathFallback {
			emit(doc)
		}
		i := sort.Search(len(p.paths), func(i int) bool { return p.paths[i].reversed >= rev })
		for ; i < len(p.paths) && strings.HasPrefix(p.paths[i].reversed, rev); i++ {
			entry := p.paths[i]
			if len(entry.reversed) == len(rev) || entry.reversed[len(rev)] == '/' {
				for _, doc := range entry.docs {
					emit(doc)
				}
			}
		}
	}
	if len(db.segments) == 0 {
		scan(&db.pathIndex, nil)
	} else {
		for _, sg := range db.segments {
			scan(&sg.pathIndex, sg.mapping)
		}
	}
	return out
}

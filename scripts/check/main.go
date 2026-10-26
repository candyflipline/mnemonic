// Command check enforces the repo rules: no commit trailers in a range, no
// direct go.mod requirement outside scripts/check/approved-deps.txt, one Ack
// literal in internal/store, and committed pushpb code that matches
// `go generate`.
//
//	go run ./scripts/check [range [go.mod]]
//
// The range defaults to origin/main..HEAD. CI passes the pushed or pull
// request range; the command is the same.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

const approvedFile = "scripts/check/approved-deps.txt"

var trailer = regexp.MustCompile(`(?im)^(co-authored-by|claude-session):`)

func main() {
	rng, gomod := "origin/main..HEAD", "go.mod"
	if len(os.Args) > 1 {
		rng = os.Args[1]
	}
	if len(os.Args) > 2 {
		gomod = os.Args[2]
	}
	var bad []string
	var err error
	for _, run := range []func() ([]string, error){
		func() ([]string, error) { return trailers(rng) },
		func() ([]string, error) { return deps(gomod) },
		acks,
		pushpbDrift,
	} {
		var more []string
		if more, err = run(); err != nil {
			break
		}
		bad = append(bad, more...)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		os.Exit(2)
	}
	for _, b := range bad {
		fmt.Fprintln(os.Stderr, b)
	}
	if len(bad) > 0 {
		os.Exit(1)
	}
}

func trailers(rng string) ([]string, error) {
	out, err := exec.Command("git", "log", "--format=%H%x00%B%x1e", rng).Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", rng, err)
	}
	var bad []string
	for _, rec := range strings.Split(string(out), "\x1e") {
		sha, body, ok := strings.Cut(strings.TrimLeft(rec, "\n"), "\x00")
		if !ok || !trailer.MatchString(body) {
			continue
		}
		bad = append(bad, fmt.Sprintf("commit %s carries a Co-Authored-By or Claude-Session trailer. Rule: no commit trailers (AGENTS.md, Rules, Commits). Amend the message and drop the trailer line.", sha))
	}
	return bad, nil
}

func deps(gomod string) ([]string, error) {
	f, err := os.Open(approvedFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	approved := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := strings.TrimSpace(sc.Text()); m != "" {
			approved[m] = true
		}
	}
	out, err := exec.Command("go", "mod", "edit", "-json", gomod).Output()
	if err != nil {
		return nil, fmt.Errorf("go mod edit -json: %w", err)
	}
	return unapproved(out, approved)
}

func unapproved(modJSON []byte, approved map[string]bool) ([]string, error) {
	var mod struct {
		Require []struct {
			Path     string
			Indirect bool
		}
	}
	if err := json.NewDecoder(bytes.NewReader(modJSON)).Decode(&mod); err != nil {
		return nil, err
	}
	var bad []string
	for _, r := range mod.Require {
		if !r.Indirect && !approved[r.Path] {
			bad = append(bad, fmt.Sprintf("go.mod requires %s directly and it is not approved. Rule: direct dependencies are listed in %s (AGENTS.md, Rules, Dependencies). Add the module to that file only with the owner's go.", r.Path, approvedFile))
		}
	}
	sort.Strings(bad)
	return bad, nil
}

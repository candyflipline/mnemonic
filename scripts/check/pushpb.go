package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const pushpbDir = "internal/wire/pushpb"

// pushpbDrift regenerates the committed pushpb code and fails when a file
// changed. protoc is not on every developer machine, so a missing protoc
// skips the check with one line; CI installs it, so the check runs there.
func pushpbDrift() ([]string, error) {
	if _, err := exec.LookPath("protoc"); err != nil {
		fmt.Fprintln(os.Stderr, "check: skipped pushpb drift (protoc not on PATH; CI runs it)")
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(pushpbDir, "*.pb.go"))
	if err != nil {
		return nil, err
	}
	before := map[string][]byte{}
	for _, f := range files {
		if before[f], err = os.ReadFile(f); err != nil {
			return nil, err
		}
	}
	if out, err := exec.Command("go", "generate", "./"+pushpbDir+"/").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go generate %s: %w\n%s", pushpbDir, err, out)
	}
	var bad []string
	for f, was := range before {
		now, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(now, was) {
			bad = append(bad, fmt.Sprintf("%s differs from what `go generate` makes from push.proto (the file is now regenerated). Rule: committed pushpb code is generated, never hand-edited. Review `git diff %s` and commit the regenerated file.", f, f))
		}
	}
	return bad, nil
}

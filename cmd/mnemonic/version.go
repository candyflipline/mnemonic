package main

import (
	"fmt"
	"runtime/debug"
)

var version, commit = "dev", ""

func init() {
	commands["version"] = func([]string) error {
		fmt.Printf("mnemonic %s (%s)\n", version, revision())
		return nil
	}
}

func revision() string {
	if commit != "" {
		return commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return s.Value[:7]
			}
		}
	}
	return "none"
}

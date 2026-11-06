package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

var version, commit = "dev", ""

func init() {
	commands["version"] = func([]string) error {
		fmt.Printf("mnemonic %s (%s)\n", stampedVersion(), revision())
		return nil
	}
}

func stampedVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return version
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

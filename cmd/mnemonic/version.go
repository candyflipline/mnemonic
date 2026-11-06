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
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" || buildSetting(info, "vcs.revision") != "" {
		return version
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}

func buildSetting(info *debug.BuildInfo, key string) string {
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

func revision() string {
	if commit != "" {
		return commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if rev := buildSetting(info, "vcs.revision"); len(rev) >= 7 {
			return rev[:7]
		}
	}
	return "none"
}

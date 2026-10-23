package index

import (
	"strconv"
	"strings"
)

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func lsDisplayLabel(explicitTitle, userMessagePreview, wireTitle, summary, slug, harness, mode, updatedAt string, totalTurns int) string {
	if strings.TrimSpace(explicitTitle) != "" {
		return explicitTitle
	}
	if label := lsCompactPreview(userMessagePreview); label != "" {
		return label
	}
	for _, candidate := range []string{wireTitle, summary, slug} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	h := lsReadableHarness(harness)
	turns := strconv.Itoa(totalTurns) + " turns"
	if totalTurns == 1 {
		turns = "1 turn"
	}
	return h + ", " + lsReadableMode(mode) + ", " + strings.TrimSpace(updatedAt) + ", " + turns
}

func lsCompactPreview(preview string) string {
	trimmed := strings.TrimSpace(preview)
	if trimmed == "" {
		return ""
	}
	firstLine := strings.TrimSpace(strings.SplitN(trimmed, "\n", 2)[0])
	if firstLine == "" {
		return ""
	}
	runes := []rune(firstLine)
	if len(runes) <= 120 {
		return firstLine
	}
	return string(runes[:120]) + "…"
}

func lsReadableHarness(harness string) string {
	switch harness {
	case "codex":
		return "Codex"
	case "claude-code":
		return "Claude Code"
	case "cursor":
		return "Cursor"
	case "hermes":
		return "Hermes"
	case "kiro-cli":
		return "Kiro CLI"
	case "kimi-cli":
		return "Kimi CLI"
	case "nac":
		return "NAC"
	case "opencode":
		return "OpenCode"
	case "pi-coding-agent":
		return "Pi Coding Agent"
	case "prime-agent":
		return "Prime Agent"
	case "qwen-code":
		return "Qwen Code"
	}
	return harness
}

func lsReadableMode(mode string) string {
	switch mode {
	case "web_search":
		return "web search"
	case "tool_calling":
		return "tool session"
	case "mcp_internal":
		return "MCP session"
	case "chat":
		return "chat"
	}
	return "session"
}

func lsParseMode(raw string) string {
	switch raw {
	case "web_search", "mcp_internal", "tool_calling":
		return raw
	}
	return "chat"
}

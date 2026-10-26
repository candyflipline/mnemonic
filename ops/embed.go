package ops

import (
	_ "embed"
	"strings"
	"text/template"
)

//go:embed launchd/com.mnemonic.push.plist
var launchdPlist string

var launchdTemplate = template.Must(template.New("launchd").Parse(launchdPlist))

// launchd does not expand ~ in ProgramArguments or StandardOutPath.
func RenderLaunchd(label, exe, home string) (string, error) {
	var b strings.Builder
	err := launchdTemplate.Execute(&b, struct{ Label, Exe, Home string }{label, exe, home})
	return b.String(), err
}

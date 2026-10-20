package ops

import _ "embed"

// LaunchdPlist is launchd/com.mnemonic.push.plist verbatim. An installer must
// rewrite /usr/local/bin/mnemonic and the /Users/USERNAME placeholders in it
// before writing it to ~/Library/LaunchAgents: launchd does not expand ~ in
// ProgramArguments or StandardOutPath.
//
//go:embed launchd/com.mnemonic.push.plist
var LaunchdPlist string

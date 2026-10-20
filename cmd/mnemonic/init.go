package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/ops"
)

const launchdLabel = "com.mnemonic.push"

// machineRe mirrors the check config.LoadClient applies, so a bad --machine
// fails here at setup instead of on the teammate's first push.
var machineRe = regexp.MustCompile(`^[a-z0-9._-]+$`)

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	server := fs.String("server", "", "base URL of the mnemonic server (required)")
	tokenFile := fs.String("token-file", "", "file holding the token minted by `mnemonic token new` (required)")
	machine := fs.String("machine", "", "machine name (default: short hostname)")
	force := fs.Bool("force", false, "overwrite an existing config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *server == "" {
		return fmt.Errorf("--server <url> is required")
	}
	if *tokenFile == "" {
		return fmt.Errorf("--token-file <path> is required")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("init: home: %w", err)
	}
	dir := filepath.Join(home, ".mnemonic")
	configPath := filepath.Join(dir, "config.toml")
	tokenPath := filepath.Join(dir, "token")

	if *machine == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("init: machine: %w", err)
		}
		if i := strings.IndexByte(host, '.'); i >= 0 {
			host = host[:i]
		}
		*machine = strings.ToLower(host)
	}
	if !machineRe.MatchString(*machine) {
		return fmt.Errorf("machine %q must match [a-z0-9._-]+", *machine)
	}
	if _, err := os.Stat(configPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass --force to overwrite it", configPath)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("init: %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("init: %s: %w", dir, err)
	}

	src, err := expandHome(*tokenFile)
	if err != nil {
		return err
	}
	if src, err = filepath.Abs(src); err != nil {
		return fmt.Errorf("init: token-file: %w", err)
	}
	if filepath.Clean(src) != filepath.Clean(tokenPath) {
		raw, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("init: token-file: %w", err)
		}
		if err := writeToken(tokenPath, raw); err != nil {
			return err
		}
		fmt.Println("wrote " + tokenPath)
	} else {
		fmt.Println("kept  " + tokenPath)
	}

	cfg := config.Client{
		Server:    *server,
		TokenFile: tokenPath,
		Machine:   *machine,
		Sources:   append([]config.SourceGlob(nil), config.DefaultSources...),
	}
	cf, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("init: %s: %w", configPath, err)
	}
	err = toml.NewEncoder(cf).Encode(cfg)
	if cerr := cf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("init: %s: %w", configPath, err)
	}
	fmt.Println("wrote " + configPath)

	exe, err := runningBinary()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return initLaunchd(home, exe, *force)
	case "linux":
		printSystemd(exe)
		return nil
	default:
		fmt.Printf("no scheduler setup for %s; run `mnemonic push` from cron or similar\n", runtime.GOOS)
		return nil
	}
}

func initLaunchd(home, exe string, force bool) error {
	agents := filepath.Join(home, "Library", "LaunchAgents")
	plistPath := filepath.Join(agents, launchdLabel+".plist")
	plist := strings.ReplaceAll(ops.LaunchdPlist, "/usr/local/bin/mnemonic", exe)
	plist = strings.ReplaceAll(plist, "/Users/USERNAME", home)
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return fmt.Errorf("init: %s: %w", agents, err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("init: %s: %w", plistPath, err)
	}
	fmt.Println("wrote " + plistPath)
	cmd := "launchctl bootstrap gui/$(id -u) " + plistPath
	if force {
		cmd = "launchctl bootout gui/$(id -u)/" + launchdLabel + " 2>/dev/null; " + cmd
	}
	fmt.Println(cmd)
	return nil
}

func printSystemd(exe string) {
	fmt.Printf(`# save as ~/.config/systemd/user/mnemonic-push.service
[Unit]
Description=mnemonic push (upload agent traces)

[Service]
Type=oneshot
ExecStart=%s push

# save as ~/.config/systemd/user/mnemonic-push.timer
[Unit]
Description=run mnemonic push every 60 s

[Timer]
OnBootSec=60
OnUnitActiveSec=60

[Install]
WantedBy=timers.target

then run:
systemctl --user daemon-reload
systemctl --user enable --now mnemonic-push.timer
`, exe)
}

func runningBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("init: binary path: %w", err)
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("init: %s: %w", exe, err)
	}
	return real, nil
}

// expandHome rewrites a leading ~ with the home directory, matching how
// config.LoadClient re-reads token_file.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~"+string(os.PathSeparator)) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("init: ~: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return home + p[1:], nil
}

// writeToken creates at 0600 and then chmods: the create mode passes through
// the umask, and the file may already exist with looser modes.
func writeToken(path string, raw []byte) error {
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("init: %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("init: %s: %w", path, err)
	}
	return nil
}

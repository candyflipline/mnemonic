package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/ops"
)

const launchdLabel = "com.mnemonic.push"

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	server := fs.String("server", "", "quic://host:port of the mnemonic server (required)")
	serverKey := fs.String("server-key", "", "the server's key, from `mnemonic key --file` on the server (required)")
	author := fs.String("author", defaultAuthor(), "author for the printed [[keys]] entry")
	machine := fs.String("machine", "", "machine name (default: short hostname)")
	force := fs.Bool("force", false, "overwrite an existing config.toml")
	label := fs.String("label", launchdLabel, "launchd label (macOS)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("init: home: %w", err)
	}
	dir := filepath.Join(home, ".mnemonic")
	configPath := filepath.Join(dir, "config.toml")

	if _, err := os.Stat(configPath); err == nil && *server == "" && *serverKey == "" && !*force {
		fmt.Println("kept  " + configPath)
		return installScheduler(home, *label)
	}
	if *server == "" {
		return fmt.Errorf("--server quic://host:port is required")
	}
	if *serverKey == "" {
		return fmt.Errorf("--server-key <key> is required")
	}

	if *machine == "" {
		host, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("init: machine: %w", err)
		}
		*machine = config.ShortHost(host)
	}
	if !wire.ValidSegment(*machine) {
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

	cfg := config.Client{
		Server:    *server,
		ServerKey: *serverKey,
		KeyFile:   config.DefaultKeyFile,
		Machine:   *machine,
		Sources:   append([]config.SourceGlob(nil), config.DefaultSources...),
	}
	if err := writeClientConfig(configPath, cfg); err != nil {
		return err
	}
	fmt.Println("wrote " + configPath)
	if err := printKey(cfg.KeyFile, *author); err != nil {
		return err
	}

	return installScheduler(home, *label)
}

func installScheduler(home, label string) error {
	exe, err := runningBinary()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return initLaunchd(home, exe, label)
	case "linux":
		printSystemd(exe)
		return nil
	default:
		fmt.Printf("no scheduler setup for %s; run `mnemonic push` from cron or similar\n", runtime.GOOS)
		return nil
	}
}

func initLaunchd(home, exe, label string) error {
	agents := filepath.Join(home, "Library", "LaunchAgents")
	plistPath := filepath.Join(agents, label+".plist")
	plist, err := ops.RenderLaunchd(label, exe, home)
	if err != nil {
		return fmt.Errorf("init: plist: %w", err)
	}
	if err := os.MkdirAll(agents, 0o755); err != nil {
		return fmt.Errorf("init: %s: %w", agents, err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("init: %s: %w", plistPath, err)
	}
	fmt.Println("wrote " + plistPath)
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if out, err := exec.Command("launchctl", "bootout", domain+"/"+label).CombinedOutput(); err != nil {
		fmt.Printf("bootout %s/%s: not loaded (%s)\n", domain, label, trimOut(out, err))
	} else {
		fmt.Println("booted out " + domain + "/" + label)
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("init: launchctl bootstrap %s %s: %s", domain, plistPath, trimOut(out, err))
	}
	fmt.Println("bootstrapped " + domain + "/" + label)
	return nil
}

func trimOut(out []byte, err error) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return err.Error()
}

func printSystemd(exe string) {
	fmt.Printf(`# save as ~/.config/systemd/user/mnemonic-push.service
[Unit]
Description=mnemonic push (upload agent traces)

[Service]
Type=simple
ExecStart=%s push --watch
Restart=always
RestartSec=60
Nice=10

[Install]
WantedBy=default.target

then run:
systemctl --user daemon-reload
systemctl --user enable --now mnemonic-push.service
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

// writeClientConfig writes cfg beside path, loads it back so a config that
// would not load is never installed, and renames it over path.
func writeClientConfig(path string, cfg config.Client) error {
	tmp := path + ".new"
	cf, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("init: %s: %w", tmp, err)
	}
	err = toml.NewEncoder(cf).Encode(cfg)
	if cerr := cf.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		_, err = config.LoadClient(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("init: %s: %w", path, err)
	}
	return nil
}

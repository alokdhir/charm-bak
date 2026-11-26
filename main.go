// Command charm-bak is the terminal UI for charm-bak backups: sources,
// snapshots, browsing snapshot trees in columns, restore and diff. It
// talks to the menu bar app, which runs restic. See AGENTS.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/backup"
	"charm-bak/internal/client"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui"
)

// version is the TUI's version, sent in hello.
const version = "0.7.0"

// appBundleID is the menu bar app (app/build.sh).
const appBundleID = "io.github.alokdhir.charm-bak"

func main() {
	fake := flag.Bool("fake", false, "browse built-in fixture data instead of the app")
	env := flag.Bool("env", false, `print the repository as shell exports, for running restic by hand: eval "$(charm-bak -env)"`)
	flag.Parse()

	if *env {
		os.Exit(printEnv())
	}

	var root *ui.Root
	if *fake {
		root = ui.NewRoot(ui.NewApp(backup.NewFake(time.Now()), time.Now))
	} else {
		root = ui.NewConnectRoot(ui.Connector{
			Dial: func(ctx context.Context) (backup.Repo, error) {
				return client.Dial(ctx, protocol.SocketPath(), version)
			},
			Launch: func() error { return exec.Command("open", "-b", appBundleID).Run() },
		}, time.Now)
	}
	if _, err := tea.NewProgram(root).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "charm-bak:", err)
		os.Exit(1)
	}
}

// printEnv passes through the bundled daemon's -env: the TUI holds no
// repository settings of its own. The output reads secrets from the
// Keychain when it's eval'ed; it never contains them.
func printEnv() int {
	daemon := ""
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			daemon = filepath.Join(filepath.Dir(exe), "charm-bak-daemon")
		}
	}
	if _, err := os.Stat(daemon); err != nil { // not run from the bundle (go run, a dev build)
		home, _ := os.UserHomeDir()
		daemon = filepath.Join(home, "Applications", "charm-bak.app", "Contents", "MacOS", "charm-bak-daemon")
	}
	cmd := exec.Command(daemon, "-env")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if x, ok := err.(*exec.ExitError); ok {
			return x.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "charm-bak:", err)
		return 1
	}
	return 0
}

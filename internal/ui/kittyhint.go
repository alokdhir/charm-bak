package ui

import (
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

// kittyLines are what kitty.conf needs for a click on a notification to
// bring this TUI's own window and tab forward (the menu bar app finds it
// with `kitten @ ls` and focuses it). socket-only: only programs that
// can open kitty's socket, which is local and the user's, may drive it.
const kittyLines = "allow_remote_control socket-only\nlisten_on unix:/tmp/kitty"

// Seams for tests: the environment, and the file that records the tip
// was shown.
var (
	kittyHintEnv  = os.Getenv
	kittyHintFile = func() string {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		return filepath.Join(dir, "charm-bak", "kitty-hint-shown")
	}
)

// kittyHint offers the kitty.conf lines once, the first time the TUI runs
// in a kitty that doesn't listen for remote control (kitty sets
// KITTY_LISTEN_ON for its programs when it does). It waits behind setup
// and any other popup.
func (a *App) kittyHint() {
	path := kittyHintFile()
	if kittyHintEnv("KITTY_WINDOW_ID") == "" || kittyHintEnv("KITTY_LISTEN_ON") != "" || path == "" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	}
	a.ask(&confirm{
		title: "Notifications and kitty",
		body: "Clicking a charm-bak notification can bring this window and tab forward. kitty allows that only with remote control on: add these lines to kitty.conf, then restart kitty.\n\n" +
			kittyLines + "\n\nsocket-only means only programs that can reach kitty's own socket on this Mac may drive it, not text printed in a terminal.",
		answers: []string{"Copy the lines", "Not now"},
		act: func(a *App, answer int) tea.Cmd {
			// Asked once, whatever the answer.
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
				os.WriteFile(path, nil, 0o600)
			}
			if answer == 0 {
				a.statusMsg = "Copied: paste them at the end of ~/.config/kitty/kitty.conf"
				return copyToClipboardCmd(kittyLines + "\n")
			}
			return nil
		},
	})
}

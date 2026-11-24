package ui

import (
	"encoding/base64"
	"os"
	"os/exec"
	"runtime"
	"time"

	tea "charm.land/bubbletea/v2"
)

// copyToClipboardCmd copies text through the terminal with OSC 52, so it
// also works over SSH and tmux, where pbcopy would only reach the remote
// machine's clipboard. It builds the sequence itself instead of calling
// tea.SetClipboard, because that ends the sequence with BEL, which some
// terminals (kitty) also ring as a bell. ESC \ is the equally valid
// terminator with no bell byte.
func copyToClipboardCmd(text string) tea.Cmd {
	return tea.Raw("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x1b\\")
}

// macCopySoundPath is a short system sound used as the copy confirmation.
const macCopySoundPath = "/System/Library/PrivateFrameworks/WorkflowEditor.framework/Versions/A/Resources/Reorder.aiff"

// audibleCueCmd confirms a copy: the system sound on macOS, the terminal
// bell anywhere that sound or afplay is missing. Pair it with every copy.
func audibleCueCmd() tea.Cmd {
	if runtime.GOOS == "darwin" {
		if path, err := exec.LookPath("afplay"); err == nil {
			if _, err := os.Stat(macCopySoundPath); err == nil {
				return func() tea.Msg {
					cmd := exec.Command(path, macCopySoundPath)
					if cmd.Start() == nil {
						go func() { _ = cmd.Wait() }()
					}
					return nil
				}
			}
		}
	}
	// tea.Raw, never tea.Printf: Printf scrolls the alt screen and eats
	// the top row for good.
	return tea.Raw("\a")
}

// focusClickGrace: clicking an unfocused terminal window both focuses it
// and delivers the click, and the two arrive together. A click this soon
// after a focus report only meant "focus the window", so it's ignored.
const focusClickGrace = 250 * time.Millisecond

package ui

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps the tests from the machine they run on: a terminal's
// own variables (kitty's) would otherwise pop the kitty tip into every
// test that starts an app.
func TestMain(m *testing.M) {
	kittyHintEnv = func(string) string { return "" }
	selectOwnWindow = func() {} // never drive the screen session the tests run in
	os.Exit(m.Run())
}

// The kitty tip shows once in a kitty that doesn't listen, and never
// again; not at all outside kitty or in one that listens.
func TestKittyHint(t *testing.T) {
	shown := filepath.Join(t.TempDir(), "shown")
	env := map[string]string{"KITTY_WINDOW_ID": "7"}
	kittyHintEnv, kittyHintFile = func(k string) string { return env[k] }, func() string { return shown }
	defer func() { kittyHintEnv = func(string) string { return "" } }()

	a := newTestApp(t, 160, 40)
	a.modal = nil
	a.kittyHint()
	c, ok := a.modal.(*confirm)
	if !ok {
		t.Fatalf("no tip: %T", a.modal)
	}
	press(a, "enter") // Copy the lines
	if _, err := os.Stat(shown); err != nil {
		t.Fatal("not recorded as shown")
	}
	if a.modal != nil || c.answers[0] != "Copy the lines" {
		t.Fatalf("after answering: %T", a.modal)
	}
	a.kittyHint()
	if a.modal != nil {
		t.Error("shown twice")
	}
	os.Remove(shown)
	env["KITTY_LISTEN_ON"] = "unix:/tmp/kitty-1"
	a.kittyHint()
	if a.modal != nil {
		t.Error("shown in a kitty that listens")
	}
}

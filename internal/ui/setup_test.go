package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// newBareApp is a machine with no repository yet, as on first launch.
func newBareApp(t *testing.T, w, h int) (*App, *backup.Fake) {
	t.Helper()
	f := backup.NewFake(testNow)
	f.NoRepository()
	a := NewApp(f, func() time.Time { return testNow })
	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	drain(a, a.Init())
	return a, f
}

func checkFrame(t *testing.T, a *App, w, h int, when string) {
	t.Helper()
	lines := strings.Split(a.View().Content, "\n")
	if len(lines) != h {
		t.Errorf("%s: %d lines, want %d", when, len(lines), h)
	}
	for _, l := range lines {
		if lw := ansi.StringWidth(l); lw > w {
			t.Errorf("%s: a line %d wide", when, lw)
		}
	}
}

// With no repository the flow opens by itself, once; a failed check
// says why and stays on the fields; the popup keeps one size throughout.
func TestSetupFirstRun(t *testing.T) {
	// Not below 100 wide: there the screen only says to resize, and keys
	// do nothing.
	for _, size := range [][2]int{{160, 40}, {100, 26}, {100, 20}} {
		a, _ := newBareApp(t, size[0], size[1])
		f, ok := a.modal.(*setupFlow)
		if !ok || f.host != "studio" || f.remote || f.step != sfWhere {
			t.Fatalf("first run: modal %T", a.modal)
		}
		height := lipglossHeight(f.view(a))
		checkFrame(t, a, size[0], size[1], "where")
		press(a, "enter") // Amazon S3
		if f.step != sfHow || len(f.howOptions()) != 2 {
			t.Fatalf("S3: step %d, %d ways to a key", f.step, len(f.howOptions()))
		}
		if h := lipglossHeight(f.view(a)); h != height {
			t.Errorf("%v: popup %d rows on how, %d on where", size, h, height)
		}
		checkFrame(t, a, size[0], size[1], "how")
		press(a, "down", "enter") // I have a key
		if f.step != sfDetails || f.backend.Fields[f.field].Key != "bucket" || f.values()["folder"] != "studio" {
			t.Fatalf("details: step %d cursor %d values %v", f.step, f.field, f.values())
		}
		checkFrame(t, a, size[0], size[1], "details")
		press(a, "enter")
		if f.note != "Bucket is needed." || f.busy != "" {
			t.Errorf("empty bucket: note %q", f.note)
		}
		press(a, "b", "down", "down", "down", "bad", "tab", "x", "enter")
		if f.step != sfDetails || !strings.Contains(f.note, "Access Key Id") {
			t.Errorf("bad key: step %d note %q", f.step, f.note)
		}
		if h := lipglossHeight(f.view(a)); h != height {
			t.Errorf("%v: popup %d rows on details, %d on where", size, h, height)
		}
		checkFrame(t, a, size[0], size[1], "failed check")
		press(a, "esc", "esc", "esc")
		if a.modal != nil {
			t.Fatalf("esc twice: %T", a.modal)
		}
		// It doesn't come back on its own; Enter on the header opens it again.
		drain(a, a.loadStatus())
		if a.modal != nil {
			t.Fatalf("reopened by itself")
		}
		cursorTo(t, a, "#"+a.localHost())
		press(a, "enter")
		if _, ok := a.modal.(*setupFlow); !ok {
			t.Fatalf("enter on the header: %T", a.modal)
		}
	}
}

// A repository already at the location asks for its password; the right
// one connects, with no recovery key step (nothing was made).
func TestSetupConnect(t *testing.T) {
	a, fake := newBareApp(t, 120, 30)
	fake.Stores["/Volumes/Backup/charm-bak"] = "hunter2"
	f := a.modal.(*setupFlow)
	press(a, "down", "down", "down", "enter")
	if f.backend.ID != "local" {
		t.Fatalf("picked %q", f.backend.ID)
	}
	press(a, "/Volumes/Backup/charm-bak", "enter")
	if f.step != sfConnect {
		t.Fatalf("step %d note %q", f.step, f.note)
	}
	press(a, "nope", "enter")
	if f.step != sfConnect || f.err == nil {
		t.Fatalf("wrong password: step %d err %v", f.step, f.err)
	}
	for range 4 {
		press(a, "backspace")
	}
	f.password.SetValue("hunter2")
	press(a, "enter")
	if f.step != sfDone || f.key != "" {
		t.Fatalf("connect: step %d key %q err %v", f.step, f.key, f.err)
	}
	if a.repoStatus.Repository == nil || a.repoStatus.Repository.Location != "/Volumes/Backup/charm-bak" {
		t.Errorf("status after connect %+v", a.repoStatus.Repository)
	}
	press(a, "enter")
	if _, ok := a.modal.(*folderPicker); !ok {
		t.Errorf("after done: %T", a.modal)
	}
}

func lipglossHeight(s string) int { return strings.Count(s, "\n") + 1 }

// A folder for a remote is on the remote, and every screen says so.
func TestSetupRemoteFolderSaysWhere(t *testing.T) {
	a, pantry := newRemoteApp(t)
	pantry.NoRepository()
	drain(a, a.loadSources())
	cursorTo(t, a, "#pantry")
	press(a, "enter")
	f := a.modal.(*setupFlow)
	if v := ansi.Strip(f.view(a)); !strings.Contains(v, "on pantry, not this Mac") {
		t.Errorf("list doesn't say whose folder:\n%s", v)
	}
	f.pick = 3
	press(a, "enter", "/srv/b", "enter")
	if f.step != sfCreate || !strings.Contains(ansi.Strip(f.view(a)), "/srv/b on pantry") {
		t.Errorf("create screen (step %d):\n%s", f.step, ansi.Strip(f.view(a)))
	}
}

// Signing in to AWS makes the bucket and the key; the flow fills them in
// and goes straight on to the check, the popup the same size throughout.
func TestSetupS3SignIn(t *testing.T) {
	a, fake := newBareApp(t, 120, 30)
	f := a.modal.(*setupFlow)
	height := lipglossHeight(f.view(a))
	press(a, "enter", "enter") // S3, sign in
	if f.step != sfDetails || f.keyFrom != keySignIn || f.backend.Fields[f.field].Key != "bucket" {
		t.Fatalf("step %d from %d on %s", f.step, f.keyFrom, f.backend.Fields[f.field].Key)
	}
	view := ansi.Strip(f.view(a))
	if strings.Contains(view, "Access key") || !strings.Contains(view, "studio/") || !strings.Contains(view, "sign in") {
		t.Errorf("sign-in details show:\n%s", view)
	}
	// Everything it will make in the account is named before Enter.
	for _, want := range []string{"charm-bak-studio", "charm-bak-manager", "charm-bak-boundary-", "Bucket charm-bak-<account number>"} {
		if !strings.Contains(view, want) {
			t.Errorf("sign-in details don't say %q:\n%s", want, view)
		}
	}
	if h := lipglossHeight(f.view(a)); h != height {
		t.Errorf("popup %d rows on sign-in details, %d on where", h, height)
	}
	press(a, "enter")
	if len(fake.AWSCalls) != 1 || !fake.AWSCalls[0].SignIn || fake.AWSCalls[0].Host != "studio" || fake.AWSCalls[0].Bucket != "" {
		t.Fatalf("aws_keys calls %+v", fake.AWSCalls)
	}
	if f.step != sfCreate || f.location != "s3:s3.amazonaws.com/charm-bak-123456789012/studio" || f.env["AWS_ACCESS_KEY_ID"] != "AKIAFAKESTUDIO" {
		t.Fatalf("after sign-in: step %d at %q env %v (note %q, err %v)", f.step, f.location, f.env, f.note, f.err)
	}
	press(a, "esc", "enter") // back to the fields and again: no second key
	if len(fake.AWSCalls) != 1 || f.step != sfCreate {
		t.Errorf("again: %d calls, step %d", len(fake.AWSCalls), f.step)
	}

	// A bucket someone else has: the reason, still on the fields.
	b, fake2 := newBareApp(t, 120, 30)
	g := b.modal.(*setupFlow)
	press(b, "enter", "enter", "taken", "enter")
	if g.step != sfDetails || g.err == nil || !strings.Contains(g.err.Error(), "someone else") {
		t.Errorf("taken: step %d err %v", g.step, g.err)
	}
	// Without the AWS CLI it says what to install.
	fake2.AWS.CLI = false
	g.aws.CLI = false
	press(b, "esc", "enter")
	if g.step != sfHow || !strings.Contains(g.note, "brew install awscli") {
		t.Errorf("no CLI: step %d note %q", g.step, g.note)
	}
}

// While the daemon makes the key, each step ticks off what's on screen
// and the busy line says what's happening now.
func TestSetupAWSSteps(t *testing.T) {
	a, _ := newBareApp(t, 120, 30)
	f := a.modal.(*setupFlow)
	press(a, "enter", "enter") // S3, sign in
	f.busy = "sign in to AWS in your browser; charm-bak waits"
	step := func(s string) {
		a.handleEvent(protocol.Event{Name: protocol.EventAWSStep, AWSStep: &protocol.AWSStepEvent{For: "studio", Step: s, Account: "123456789012"}})
	}
	step(protocol.AWSStepSignedIn)
	if !strings.Contains(f.busy, "signed in") || f.awsDone != 0 {
		t.Errorf("signed in: %q %d", f.busy, f.awsDone)
	}
	step(protocol.AWSStepBucket)
	step(protocol.AWSStepManager)
	if f.awsDone != 2 || !strings.Contains(f.busy, "charm-bak-studio") {
		t.Errorf("manager: %q %d", f.busy, f.awsDone)
	}
	if got := strings.Count(ansi.Strip(f.view(a)), "✓ "); got != 2 {
		t.Errorf("%d ticks, want 2", got)
	}
	// Another machine's steps aren't this flow's.
	a.handleEvent(protocol.Event{Name: protocol.EventAWSStep, AWSStep: &protocol.AWSStepEvent{For: "wren", Step: protocol.AWSStepUser}})
	if f.awsDone != 2 {
		t.Errorf("took wren's step: %d", f.awsDone)
	}
}

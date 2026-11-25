package ui

import (
	"charm.land/lipgloss/v2"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// A running job shows at the footer's left end; ^T lists it, and c
// cancels it after asking. With nothing running, ^T just says so.
func TestTaskIndicatorAndList(t *testing.T) {
	f := backup.NewFake(testNow)
	a := NewApp(f, func() time.Time { return testNow })
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	drain(a, a.Init())

	press(a, "ctrl+t")
	if a.modal != nil || a.statusMsg != "Nothing is running" {
		t.Fatalf("idle ^T: modal %T, status %q", a.modal, a.statusMsg)
	}
	src := a.sources[0]
	a.job = &protocol.Job{ID: "j-1", Kind: protocol.KindBackup, Source: &src.ID, Started: testNow.Add(-time.Minute)}
	a.progress = &protocol.Progress{JobID: "j-1", Percent: 0.42, FilesDone: 10, TotalFiles: 100}
	a.statusMsg = ""
	foot := ansi.Strip(a.renderFooter())
	if !strings.Contains(foot, "backing up "+src.Name+" 42%") || !strings.Contains(foot, "^T") {
		t.Errorf("footer %q", foot)
	}
	press(a, "ctrl+t")
	if _, ok := a.modal.(*taskList); !ok {
		t.Fatalf("^T: modal %T", a.modal)
	}
	view := ansi.Strip(a.modal.view(a))
	if !strings.Contains(view, "42% · 10 of 100 files") {
		t.Errorf("task list:\n%s", view)
	}
	press(a, "c")
	press(a, "left", "enter")
	if len(f.Cancelled) != 1 || f.Cancelled[0] != "j-1" {
		t.Errorf("cancelled %v", f.Cancelled)
	}
}

// ^N lists failures and finished clean-ups from history, newest first;
// Enter goes to the source.
func TestNoticeList(t *testing.T) {
	f := backup.NewFake(testNow)
	a := NewApp(f, func() time.Time { return testNow })
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	drain(a, a.Init())
	src := a.sources[1]
	f.AddRun(protocol.Run{Job: "j-a", Kind: protocol.KindBackup, Source: &src.ID, Started: testNow.Add(-2 * time.Hour), Ended: testNow.Add(-2 * time.Hour), Status: protocol.RunFailed, Error: "bucket said no"})
	f.AddRun(protocol.Run{Job: "j-b", Kind: protocol.KindRewrite, Source: &src.ID, Started: testNow.Add(-time.Hour), Ended: testNow.Add(-time.Hour), Status: protocol.RunOK})
	f.AddRun(protocol.Run{Job: "j-c", Kind: protocol.KindBackup, Source: &src.ID, Started: testNow.Add(-time.Hour), Ended: testNow.Add(-30 * time.Minute), Status: protocol.RunOK})
	f.AddRun(protocol.Run{Job: "j-d", Kind: protocol.KindBackup, Source: &src.ID, Started: testNow.Add(-20 * time.Minute), Ended: testNow.Add(-10 * time.Minute), Status: protocol.RunIncomplete, Error: "Couldn't read 1 file: /x (in use).", Skipped: 1})

	press(a, "ctrl+n")
	m, ok := a.modal.(*noticeList)
	if !ok || m.loading {
		t.Fatalf("^N: %T", a.modal)
	}
	if len(m.list) < 3 || m.list[0].run.Job != "j-d" || m.list[1].run.Job != "j-b" || m.list[2].run.Job != "j-a" {
		t.Fatalf("list %+v", m.list)
	}
	view := ansi.Strip(m.view(a))
	for _, want := range []string{"Past snapshots of " + src.Name + " cleaned up", "Backup of " + src.Name + " failed", "bucket said no", "⚠ Backup of " + src.Name + ": 1 file skipped", "/x (in use)"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	// Enter opens the details: every skipped file, fetched for the run;
	// esc goes back to the list, Enter from there to the source.
	f.SkippedFiles = map[string][]string{"j-d": {"/x (in use)", "/y (unsupported file type \"irregular\")"}}
	press(a, "enter")
	d, ok := a.modal.(*diffView)
	if !ok || d.back != m {
		t.Fatalf("details: %T", a.modal)
	}
	drain(a, a.openNotice(m, m.list[0]))
	d = a.modal.(*diffView)
	text := ansi.Strip(d.view(a))
	for _, want := range []string{"Backup of " + src.Name + ": 1 file skipped", "Couldn't read 2 files:", "• /x (in use)", "• /y (unsupported file type \"irregular\")", "enter go to its source", "esc back"} {
		if !strings.Contains(text, want) {
			t.Errorf("details lack %q:\n%s", want, text)
		}
	}
	// As tall as its content, not the whole screen.
	if h, full := lipgloss.Height(d.view(a)), a.bodyHeight(); h >= full-2 {
		t.Errorf("details %d rows of %d", h, full)
	}
	// A run from before runs kept their files: the ones its reason names.
	old := protocol.Run{Job: "j-old", Status: protocol.RunIncomplete, Skipped: 80,
		Error: "Couldn't read 80 files: C:\\a (irregular); C:\\b (irregular); and 78 more (the daemon's log lists up to 20)."}
	got := ansi.Strip(strings.Join(noticeLines(old, nil, 100), "\n"))
	if !strings.Contains(got, "Couldn't read 80 files (the first 2):") || !strings.Contains(got, "• C:\\a (irregular)") || strings.Contains(got, "78 more") {
		t.Errorf("old run:\n%s", got)
	}
	press(a, "esc")
	if a.modal != m {
		t.Fatalf("esc from details: %T", a.modal)
	}
	press(a, "enter")
	drain(a, a.openNotice(m, m.list[0]))
	press(a, "enter")
	if a.modal != nil {
		t.Fatal("still open")
	}
	if s, ok := a.selSource(); !ok || s.ID != src.ID {
		t.Errorf("selected %+v", s)
	}
}

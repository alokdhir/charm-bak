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

	press(a, "ctrl+n")
	m, ok := a.modal.(*noticeList)
	if !ok || m.loading {
		t.Fatalf("^N: %T", a.modal)
	}
	if len(m.list) != 2 || m.list[0].run.Job != "j-b" || m.list[1].run.Job != "j-a" {
		t.Fatalf("list %+v", m.list)
	}
	view := ansi.Strip(m.view(a))
	for _, want := range []string{"Past snapshots of " + src.Name + " cleaned up", "Backup of " + src.Name + " failed", "bucket said no"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	press(a, "enter")
	if a.modal != nil {
		t.Fatal("still open")
	}
	if s, ok := a.selSource(); !ok || s.ID != src.ID {
		t.Errorf("selected %+v", s)
	}
}

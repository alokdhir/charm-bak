package ui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// openNotice opens one notification's details over ^N: what ran, when,
// on which machine, and the whole reason; for a backup that skipped
// files, every one of them with why (fetched now: history leaves them
// out). Esc goes back to the list, Enter to the source.
func (a *App) openNotice(list *noticeList, n notice) tea.Cmd {
	key := "run\x00" + n.host + "\x00" + n.run.Job
	d := &diffView{loading: true, key: key, title: a.noticeTitle(n), sub: a.noticeSub(n),
		back: list, loadingText: "reading the details…"}
	if n.run.Source != nil {
		host, id := n.host, *n.run.Source
		d.enter = func() tea.Cmd {
			a.modal = nil
			return a.goToSource(host, id)
		}
	}
	a.modal = d
	repo := a.repo
	if n.host != a.localHost() {
		repo = a.repo.ForHost(n.host)
	}
	width := diffInnerWidth(a.width)
	run := n.run
	return func() tea.Msg {
		res, err := repo.RunDetail(context.Background(), run.Job)
		if err != nil {
			// The list's copy still says what happened.
			return diffLoadedMsg{key: key, lines: noticeLines(run, nil, width)}
		}
		return diffLoadedMsg{key: key, lines: noticeLines(res.Run, res.SkippedFiles, width)}
	}
}

// noticeSub is the details' subtitle: when it ended and where.
func (a *App) noticeSub(n notice) string {
	parts := []string{n.run.Ended.Local().Format("Mon Jan 2 15:04") + " (" + agoText(a.now(), n.run.Ended) + ")"}
	if len(a.hosts) > 1 {
		parts = append(parts, "on "+n.host)
	}
	return strings.Join(parts, " · ")
}

// noticeLines is the details' body, wrapped to width: the run's figures,
// then the reason, or the skipped files one by one.
func noticeLines(r protocol.Run, skipped []string, width int) []string {
	label, value := styles.FieldLabelStyle().Render, styles.FieldValueStyle().Render
	rows := [][2]string{{label("Took"), value(took(&r))}}
	if r.Kind == protocol.KindBackup && r.Snapshot != "" {
		rows = append(rows, [2]string{label("Snapshot"), value(fmt.Sprintf("%s: %d new, %d changed files, %s added",
			shortID(r.Snapshot), r.FilesNew, r.FilesChanged, humanBytes(r.BytesAdded)))})
	}
	// As wide as they are: a table stretched to the popup puts the
	// values half a screen from their labels.
	lines := strings.Split(renderRows(rows, 0), "\n")
	lines = append(lines, "")
	text := lipgloss.NewStyle().Width(width)
	if len(skipped) == 0 && r.Status == protocol.RunIncomplete {
		skipped = skippedFromReason(r.Error)
	}
	if len(skipped) > 0 {
		n := max(r.Skipped, int64(len(skipped)))
		head := fmt.Sprintf("Couldn't read %s:", pluralize(int(n), "file"))
		if int64(len(skipped)) < n {
			head = fmt.Sprintf("Couldn't read %s (the first %d):", pluralize(int(n), "file"), len(skipped))
		}
		lines = append(lines, styles.FieldValueStyle().Render(head))
		hang := lipgloss.NewStyle().Width(width - 2)
		for _, f := range skipped {
			for i, l := range strings.Split(hang.Render(f), "\n") {
				lead := "• "
				if i > 0 {
					lead = "  "
				}
				lines = append(lines, styles.MutedStyle().Render(lead)+styles.FieldValueStyle().Render(l))
			}
		}
		return lines
	}
	why := r.Error
	if why == "" {
		why = "Finished."
	}
	for _, l := range strings.Split(text.Render(why), "\n") {
		lines = append(lines, styles.FieldValueStyle().Render(l))
	}
	return lines
}

// skippedFromReason recovers the files named in an incomplete run's
// reason ("Couldn't read 80 files: a (why); b (why); and 77 more."), for
// runs from before each run kept its whole list.
func skippedFromReason(reason string) []string {
	_, list, ok := strings.Cut(strings.TrimSuffix(reason, "."), "files: ")
	if !ok {
		if _, list, ok = strings.Cut(strings.TrimSuffix(reason, "."), "file: "); !ok {
			return nil
		}
	}
	var files []string
	for _, f := range strings.Split(list, "; ") {
		if strings.HasPrefix(f, "and ") && strings.Contains(f, " more") {
			continue
		}
		files = append(files, f)
	}
	return files
}

// shortID is a snapshot ID as restic prints it: the first 8 characters.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

package ui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// startExclude asks whether to leave the selected file or folder out of
// its source's backups from the next run on, as if unticked in the
// source's picker. Snapshots already taken keep it.
func (a *App) startExclude() tea.Cmd {
	src, ok := a.selSource()
	if !ok {
		return nil
	}
	n, ok := a.selectedEntry()
	if !ok || n.path == "" {
		return nil
	}
	sel, patterns := backup.FromSource(src.Paths, src.Excludes)
	shown := shortPath(a.homeFor(src.Host), n.path)
	if n.IsDir() {
		shown += "/"
	}
	if !sel.Included(n.path) {
		// Excluded already: what's left is taking it out of the past.
		a.statusMsg = shown + " is already excluded from " + src.Name + "; counting the past snapshots that have it…"
		repo := a.srcRepo()
		return func() tea.Msg {
			res, err := repo.RewriteSource(context.Background(), protocol.SourceRewriteParams{ID: src.ID, Excludes: []string{n.path}, DryRun: true})
			return excludedMsg{source: src, shown: shown, path: n.path, already: true, past: true, count: res.Snapshots, countErr: err}
		}
	}
	sel.Exclude(n.path) // not Toggle: a folder with something already excluded inside would come wholly back in
	paths, excludes := sel.Compile()
	if len(paths) == 0 {
		a.statusMsg = "That's everything " + src.Name + " backs up; remove the source instead (x in Sources)"
		return nil
	}
	excludes = append(excludes, patterns...)
	repo := a.srcRepo()
	a.modal = &confirm{
		title:   "Exclude from " + src.Name,
		body:    "Leave " + shown + " out of " + src.Name + "'s backups from the next run on (e on the source undoes it).\n\nSnapshots already taken keep it, unless you also remove it from them: that frees the space, and it can't be restored afterwards.",
		answers: []string{"From now on", "Also from past snapshots", "Cancel"},
		act: func(a *App, answer int) tea.Cmd {
			if answer == 2 {
				return nil
			}
			p := protocol.SourceSetParams{ID: src.ID, Paths: paths, Excludes: &excludes}
			past := answer == 1
			if past {
				a.statusMsg = "Excluding " + shown + "; counting the snapshots that have it…"
			}
			return func() tea.Msg {
				s, err := repo.SetSource(context.Background(), p)
				msg := excludedMsg{source: s, shown: shown, path: n.path, err: err}
				if err == nil && past {
					res, err := repo.RewriteSource(context.Background(), protocol.SourceRewriteParams{ID: src.ID, Excludes: []string{n.path}, DryRun: true})
					msg.past, msg.count, msg.countErr = true, res.Snapshots, err
				}
				return msg
			}
		},
	}
	return nil
}

type excludedMsg struct {
	source protocol.Source
	shown  string
	path   string
	err    error

	already  bool // it was excluded before: only the past was asked about
	past     bool // also from past snapshots: count is how many have it
	count    int
	countErr error
}

type rewriteStartedMsg struct {
	source string
	shown  string
	count  int
	err    error
}

func (a *App) excluded(msg excludedMsg) tea.Cmd {
	if msg.err != nil {
		a.statusMsg = "Couldn't exclude " + msg.shown + ": " + strings.TrimSpace(msg.err.Error())
		return nil
	}
	a.statusMsg = "Excluded " + msg.shown + " from " + msg.source.Name + " from the next run on"
	if msg.already {
		a.statusMsg = msg.shown + " is already excluded from " + msg.source.Name
	}
	load := a.loadSources()
	switch {
	case !msg.past:
		return load
	case msg.countErr != nil:
		a.statusMsg += "; couldn't count its past snapshots: " + strings.TrimSpace(msg.countErr.Error())
		return load
	case msg.count == 0:
		a.statusMsg += "; no past snapshot has it"
		return load
	}
	repo, src, n := a.repo.ForHost(msg.source.Host), msg.source, msg.count
	a.ask(&confirm{
		title:   "Remove from past snapshots",
		body:    "Remove " + msg.shown + " from " + pluralize(n, "snapshot") + " of " + src.Name + "? It's deleted from those backups for good, and the space is freed right after.\n\nWhile it runs, " + src.Host + "'s backups wait and its snapshots can't be browsed (it locks the repository). A notification says when it's done.",
		answers: []string{"Remove from " + pluralize(n, "snapshot"), "Cancel"},
		pick:    1,
		danger:  map[int]bool{0: true},
		act: func(a *App, answer int) tea.Cmd {
			if answer != 0 {
				return nil
			}
			return func() tea.Msg {
				_, err := repo.RewriteSource(context.Background(), protocol.SourceRewriteParams{ID: src.ID, Excludes: []string{msg.path}})
				return rewriteStartedMsg{src.Name, msg.shown, n, err}
			}
		},
	})
	return load
}

func (a *App) rewriteStarted(msg rewriteStartedMsg) tea.Cmd {
	if msg.err != nil {
		a.statusMsg = "Couldn't remove " + msg.shown + " from " + msg.source + "'s snapshots: " + strings.TrimSpace(msg.err.Error())
		return nil
	}
	a.statusMsg = "Removing " + msg.shown + " from " + pluralize(msg.count, "snapshot") + " of " + msg.source + "; a notification says when it's done"
	return nil
}

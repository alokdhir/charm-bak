package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
)

// Adopting a machine's AWS key (internal/daemon/adopt.go): one charm-bak
// didn't make, on a machine whose repository is in S3, brought under its
// care so it's rotated every 90 days and removing the machine deletes its
// IAM user. The TUI offers it once per machine; the header's Adopt key
// does it any time.

// adoptable reports whether h uses an AWS key charm-bak could adopt.
func (a *App) adoptable(h protocol.Host) bool {
	if h.KeyMade != nil {
		return false
	}
	res, ok := a.repos[h.Name]
	return ok && res.Repository != nil && strings.HasPrefix(res.Repository.Location, "s3:") && res.Repository.AWSFrom != ""
}

// adoptOfferedFile records that the offer for host was made (whatever
// the answer), so it's made once; a seam for tests.
var adoptOfferedFile = func(host string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "charm-bak", "adopt-offered-"+host)
}

// offerAdopt asks, once per machine, whether to adopt the keys waiting:
// every one in one question and one sign-in. It waits behind setup and
// any other popup.
func (a *App) offerAdopt() {
	var hosts []protocol.Host
	var files []string
	for _, h := range a.hosts {
		if !a.adoptable(h) || a.adoptAsked[h.Name] {
			continue
		}
		path := adoptOfferedFile(h.Name)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if a.adoptAsked == nil {
			a.adoptAsked = map[string]bool{}
		}
		a.adoptAsked[h.Name] = true
		hosts, files = append(hosts, h), append(files, path)
	}
	if len(hosts) > 0 {
		a.ask(a.adoptConfirm(hosts, files))
	}
}

// many is how a question names several machines: "both" for two, "all
// 3" from three on.
func many(n int) string {
	if n == 2 {
		return "both"
	}
	return fmt.Sprintf("all %d", n)
}

// adoptConfirm is the question, for one machine or several (one
// sign-in either way); offered names the files that record it was
// offered, nil when asked for from a header.
func (a *App) adoptConfirm(hosts []protocol.Host, offered []string) *confirm {
	var names, wire []string
	for _, h := range hosts {
		names = append(names, h.Name)
		if h.Local {
			wire = append(wire, "")
		} else {
			wire = append(wire, h.Name)
		}
	}
	title := "Adopt " + names[0] + "'s AWS key?"
	body := "charm-bak didn't make the key " + names[0] + " uses, so it doesn't rotate it. Adopted, it's rotated every 90 days like the keys charm-bak makes, " +
		"and removing " + names[0] + " with its backups deletes its IAM user. You sign in to AWS once, in your browser: the user charm-bak-" + names[0] +
		" moves under /charm-bak/ and is limited to its own folder in the bucket. Its key stays the same until the first rotation."
	yes := "Adopt it"
	if len(hosts) > 1 {
		title = "Adopt " + many(len(hosts)) + " machines' AWS keys?"
		body = joinAnd(names) + " use AWS keys charm-bak didn't make, so it doesn't rotate them. Adopted, they're rotated every 90 days like the keys charm-bak makes, " +
			"and removing a machine with its backups deletes its IAM user. You sign in to AWS once, in your browser, for " + many(len(hosts)) +
			": each machine's charm-bak-<name> user moves under /charm-bak/ and is limited to its own folder in the bucket. The keys stay the same until their first rotation."
		yes = "Adopt " + many(len(hosts))
	}
	return &confirm{
		title:   title,
		body:    body,
		answers: []string{yes, "Not now"},
		act: func(a *App, answer int) tea.Cmd {
			for _, f := range offered {
				if err := os.MkdirAll(filepath.Dir(f), 0o700); err == nil {
					os.WriteFile(f, nil, 0o600)
				}
			}
			if answer != 0 {
				if offered != nil {
					a.statusMsg = "Adopt key, on a machine's header, does it later"
				}
				return nil
			}
			a.statusMsg = "sign in to AWS in your browser to adopt " + joinAnd(names) + "'s keys…"
			if len(names) == 1 {
				a.statusMsg = "sign in to AWS in your browser to adopt " + names[0] + "'s key…"
			}
			repo := a.repo
			return func() tea.Msg {
				res, err := repo.AWSAdopt(context.Background(), wire)
				return adoptedMsg{res, err}
			}
		},
	}
}

// startAdopt is the header's Adopt key: that machine alone.
func (a *App) startAdopt() tea.Cmd {
	h, ok := a.selHost()
	if !ok || !a.adoptable(h) {
		return nil
	}
	a.modal = a.adoptConfirm([]protocol.Host{h}, nil)
	return nil
}

type adoptedMsg struct {
	res protocol.AWSAdoptResult
	err error
}

// adopted says how it went; the machines' key ages arrive with hosts.
func (a *App) adopted(msg adoptedMsg) tea.Cmd {
	var failed []string
	for _, f := range msg.res.Failed {
		failed = append(failed, f.Host+": "+f.Error)
	}
	if msg.err != nil {
		failed = append(failed, msg.err.Error())
	}
	var done []string
	for _, k := range msg.res.Adopted {
		done = append(done, k.Host)
	}
	a.statusMsg = ""
	if len(done) > 0 {
		a.statusMsg = joinAnd(done) + ": adopted; charm-bak rotates " + map[bool]string{true: "its key", false: "their keys"}[len(done) == 1] + " from now on"
	}
	if len(failed) > 0 {
		a.ask(&confirm{title: "Couldn't adopt every key", body: strings.Join(failed, "\n\n"), answers: []string{"OK"},
			act: func(*App, int) tea.Cmd { return nil }})
	}
	return a.loadSources()
}

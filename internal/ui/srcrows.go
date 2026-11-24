package ui

import (
	"fmt"
	"image/color"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// srcRow is a row of the Sources column: a host header (src < 0) or a
// source (an index into a.sources). The cursor stops on headers too:
// Enter folds them, and they hold the machine's repository details and
// Edit (its defaults). Even one machine has its header (Alok, 2026-11-24),
// so those have a place.
type srcRow struct {
	host string
	src  int
}

func (r srcRow) header() bool { return r.src < 0 }

// grouped reports whether the Sources column shows host headers.
func (a *App) grouped() bool { return len(a.hosts) > 0 }

// srcRows lists the Sources column's rows in display order. srcCur.pos
// indexes this list.
func (a *App) srcRows() []srcRow {
	if !a.grouped() {
		rows := make([]srcRow, len(a.sources))
		for i := range a.sources {
			rows[i] = srcRow{host: a.sources[i].Host, src: i}
		}
		return rows
	}
	var rows []srcRow
	for _, h := range a.hosts {
		rows = append(rows, srcRow{host: h.Name, src: -1})
		if a.collapsed[h.Name] {
			continue
		}
		for i, s := range a.sources {
			if s.Host == h.Name {
				rows = append(rows, srcRow{host: h.Name, src: i})
			}
		}
	}
	return rows
}

// selRow is the row under the Sources cursor.
func (a *App) selRow() (srcRow, bool) {
	rows := a.srcRows()
	if a.srcCur.pos < 0 || a.srcCur.pos >= len(rows) {
		return srcRow{}, false
	}
	return rows[a.srcCur.pos], true
}

// selSource is the selected source; false on a host header or an empty
// list.
func (a *App) selSource() (protocol.Source, bool) {
	r, ok := a.selRow()
	if !ok || r.header() {
		return protocol.Source{}, false
	}
	return a.sources[r.src], true
}

// onHeader reports whether the Sources cursor is on a host header.
func (a *App) onHeader() bool {
	r, ok := a.selRow()
	return ok && r.header()
}

// selHost is the host under the cursor when it's on a header.
func (a *App) selHost() (protocol.Host, bool) {
	r, ok := a.selRow()
	if !ok || !r.header() {
		return protocol.Host{}, false
	}
	for _, h := range a.hosts {
		if h.Name == r.host {
			return h, true
		}
	}
	return protocol.Host{}, false
}

// rowKey identifies a row across reloads: a source by sourceKey, a
// header by "#" and its host (host names can't contain "#").
func (a *App) rowKey(r srcRow) string {
	if r.header() {
		return "#" + r.host
	}
	return sourceKey(a.sources[r.src])
}

func (a *App) selectedRowKey() string {
	r, ok := a.selRow()
	if !ok {
		return ""
	}
	return a.rowKey(r)
}

// rowIndex finds a row by rowKey, -1 if it isn't showing.
func (a *App) rowIndex(key string) int {
	if key == "" {
		return -1
	}
	for i, r := range a.srcRows() {
		if a.rowKey(r) == key {
			return i
		}
	}
	return -1
}

// hostSourceCount is how many sources a host has.
func (a *App) hostSourceCount(host string) int {
	n := 0
	for _, s := range a.sources {
		if s.Host == host {
			n++
		}
	}
	return n
}

// foldHost folds (collapse) or unfolds the host header under the cursor;
// the cursor stays on the header.
func (a *App) foldHost(collapse bool) tea.Cmd {
	h, ok := a.selHost()
	if !ok || a.collapsed[h.Name] == collapse {
		return nil
	}
	if a.collapsed == nil {
		a.collapsed = map[string]bool{}
	}
	a.collapsed[h.Name] = collapse
	a.srcCur.pos = a.rowIndex("#" + h.Name)
	a.srcCur.move(0, len(a.srcRows()), a.listHeight())
	return nil
}

// addHost is where a new source goes: the selected row's host when it's
// answering (Add on pantry's header or one of its sources adds on
// pantry), else this machine.
func (a *App) addHost() string {
	if r, ok := a.selRow(); ok && r.host != "" && r.host != a.localHost() && a.findHost(r.host).State == protocol.HostOK {
		return r.host
	}
	return a.localHost()
}

// isRemote reports whether host is another machine than this one.
func (a *App) isRemote(host string) bool { return host != "" && host != a.localHost() }

// homeFor is the home folder paths are shown relative to (~): this
// machine's, none for a remote (its daemon runs as root, so / is home).
func (a *App) homeFor(host string) string {
	if a.isRemote(host) {
		return ""
	}
	home, _ := os.UserHomeDir()
	return home
}

// hostConnecting: the daemon says so until its first try at a remote ends.
const hostConnecting = "connecting"

// hostMark is a host's link as one symbol: ✓ connected, the spinner's
// frame (yellow) while connecting, ✗ unreachable.
func hostMark(h protocol.Host, spin string) (mark string, c color.Color) {
	switch {
	case h.State == protocol.HostOK:
		return "✓", styles.OK
	case h.Error == hostConnecting:
		return spin, styles.Warn
	}
	return "✗", styles.Fail
}

// hostState is a host's link for the hero, in a word: connected,
// connecting or offline (Alok, 2026-11-24: the dial error was a huge
// string that squeezed the cells beside it). Why is in the info popup
// (hostWhy).
func hostState(h protocol.Host, spin string) (text string, c color.Color) {
	mark, c := hostMark(h, spin)
	switch {
	case h.State == protocol.HostOK:
		return mark + " connected", c
	case h.Error == hostConnecting:
		return mark + " connecting", c
	}
	return mark + " offline", c
}

// hostWhy is the link in full for the info popup: the state, plus the
// connection error when there is one.
func hostWhy(h protocol.Host, spin string) string {
	text, _ := hostState(h, spin)
	if h.State != protocol.HostOK && h.Error != "" && h.Error != hostConnecting {
		text += ": " + h.Error
	}
	return text
}

func (a *App) findHost(name string) protocol.Host {
	for _, h := range a.hosts {
		if h.Name == name {
			return h
		}
	}
	return protocol.Host{Name: name}
}

// hostHeroCells is the hero's first row on a host header.
func (a *App) hostHeroCells(name string) []heroCell {
	h := a.findHost(name)
	where := seg{text: "this Mac", style: styles.FieldValueStyle()}
	if !h.Local {
		where = seg{text: h.Address, style: styles.FieldValueStyle(), stop: "Address"}
	}
	cells := []heroCell{
		{label: "Host", value: seg{text: h.Name, style: styles.FileTitleStyle(), stop: "Info"}},
		{label: "Sources", value: seg{text: fmt.Sprint(a.hostSourceCount(h.Name)), style: styles.FieldValueStyle()}},
		{label: "Where", value: where},
	}
	// Its repository, in the same grid column on every header; the rest
	// of it (credentials, upkeep, the defaults) is in the info popup.
	repo := "none yet"
	if r, ok := a.repos[h.Name]; ok && r.Repository != nil {
		repo = shortLocation(r.Repository.Location)
	} else if !ok {
		repo = "…"
	}
	cells = append(cells, heroCell{label: "Repository", value: seg{text: repo, style: styles.FieldValueStyle()}})
	// No Link cell: the second row's Daemon says offline or connecting
	// for a remote, and saying it twice was noise (Alok, 2026-11-24). The
	// info popup's Link has the reason.
	return cells
}

// shortLocation is a repository location for the hero: AWS's S3 endpoint
// reads as "S3 bucket/folder"; anything else as it is.
func shortLocation(loc string) string {
	if rest, ok := strings.CutPrefix(loc, "s3:"); ok {
		if host, path, ok := strings.Cut(rest, "/"); ok && strings.HasSuffix(host, "amazonaws.com") {
			return "S3 " + path
		}
	}
	return loc
}

// maintText is when a cleanup or check last ran, and how it went.
func (a *App) maintText(run *protocol.Run) string {
	switch {
	case run == nil:
		return "never"
	case run.Status == protocol.RunOK:
		return "✓ " + agoText(a.now(), run.Ended)
	}
	return "✗ " + agoText(a.now(), run.Ended) + ": " + run.Error
}

// hostInfoFields is the info popup for a host header.
func (a *App) hostInfoFields(h protocol.Host) []infoField {
	state := hostWhy(h, a.spinFrame())
	f := []infoField{{"Host", h.Name}, {"Sources", fmt.Sprint(a.hostSourceCount(h.Name))}}
	if h.Local {
		f = append(f, infoField{"Where", "this Mac"})
	} else {
		f = append(f, infoField{"Address", h.Address})
	}
	if !h.Local {
		f = append(f, infoField{"Link", state})
	}
	if h.LastSeen != nil {
		f = append(f, infoField{"Last seen", stamp(a.now(), *h.LastSeen)})
	}
	res, ok := a.repos[h.Name]
	if !ok {
		return f
	}
	f = append(f, infoField{})
	if r := res.Repository; r != nil {
		f = append(f, infoField{"Repository", r.Location}, infoField{"Password", r.PasswordFrom})
		if r.AWSFrom != "" {
			f = append(f, infoField{"AWS key", r.AWSFrom})
		}
		if r.StoredBytes > 0 {
			f = append(f, infoField{"Stored", humanBytes(r.StoredBytes) + " (" + humanBytes(r.LogicalBytes) + " uncompressed)"})
		}
		f = append(f, infoField{"Cleaned up", a.maintText(r.LastPrune)}, infoField{"Checked", a.maintText(r.LastCheck)})
		if r.NextMaintenance != nil && r.NextMaintenance.After(a.now()) {
			f = append(f, infoField{"Next upkeep", r.NextMaintenance.Local().Format("Mon Jan 2") + ", when nothing else is queued"})
		}
	} else {
		f = append(f, infoField{"Repository", "none yet"})
	}
	d := res.Defaults
	every := int64(0)
	if d.Schedule != nil {
		every = d.Schedule.Every
	}
	f = append(f, infoField{}, infoField{"Default schedule", a.scheduleText(every)}, infoField{"Default retention", retentionReach(d.Retention, false)})
	if len(d.Excludes) > 0 {
		f = append(f, infoField{"Default skips", strings.Join(d.Excludes, ", ")})
	}
	return f
}

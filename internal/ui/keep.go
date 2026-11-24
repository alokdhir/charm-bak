package ui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// keepPreset is a named retention: what Keep offers instead of six
// counts (Alok, 2026-11-24: per-period counts are fussier than anyone
// needs; Time Machine and Backblaze ask at most one question). Each thins
// out like Time Machine: every backup for a day, then dailies, weeklies,
// monthlies. Periods are calendar ones and restic keeps the newest
// snapshot in each.
type keepPreset struct {
	name string
	r    protocol.Retention
	help string
}

var keepPresets = []keepPreset{
	{"1 month", protocol.Retention{Last: 10, Hourly: 24, Daily: 30},
		"Every backup from the last day, then one a day for a month."},
	{"1 year", protocol.Retention{Last: 10, Hourly: 24, Daily: 30, Weekly: 12, Monthly: 12},
		"Every backup from the last day, then one a day for a month, one a week for 3 months, one a month for a year."},
	{"3 years", protocol.Retention{Last: 10, Hourly: 24, Daily: 30, Weekly: 12, Monthly: 36},
		"Every backup from the last day, then one a day for a month, one a week for 3 months, one a month for 3 years."},
	{"forever", protocol.Retention{Last: 10, Hourly: 24, Daily: 30, Weekly: 12, Monthly: 36, Yearly: protocol.KeepUnlimited},
		"Every backup from the last day, then one a day for a month, one a week for 3 months, one a month for 3 years, then one a year, never deleted."},
}

// presetFor is r's preset, -1 when it's custom counts.
func presetFor(r protocol.Retention) int {
	for i, p := range keepPresets {
		if p.r == r {
			return i
		}
	}
	return -1
}

// keepLabels and keepMeanings name the custom counts, newest first.
var (
	keepLabels   = [6]string{"newest", "hours", "days", "weeks", "months", "years"}
	keepMeanings = [6]string{
		"always the most recent ones",
		"the last of each hour",
		"the last of each day",
		"the last of each week (Mon–Sun)",
		"the last of each month",
		"the last of each year",
	}
)

// keepEditor is the Keep field: an optional "default", the presets, then
// custom, which shows the six counts.
type keepEditor struct {
	withDefault bool
	def         protocol.Retention // what "default" means
	choice      int                // index into options()
	counts      [6]textinput.Model
}

// newKeepEditor starts at r, or at "default" when inherit (withDefault).
func newKeepEditor(r, def protocol.Retention, withDefault, inherit bool) *keepEditor {
	k := &keepEditor{withDefault: withDefault, def: def}
	for i, v := range retentionValues(r) {
		text := strconv.Itoa(v)
		if v == protocol.KeepUnlimited {
			text = "∞"
		}
		k.counts[i] = newTextField(text, "0")
	}
	off := 0
	if withDefault {
		off = 1
	}
	switch p := presetFor(r); {
	case withDefault && inherit:
		k.choice = 0
	case p >= 0:
		k.choice = off + p
	default:
		k.choice = off + len(keepPresets) // custom
	}
	return k
}

func (k *keepEditor) options() int {
	n := len(keepPresets) + 1
	if k.withDefault {
		n++
	}
	return n
}

func (k *keepEditor) cycle(delta int) {
	n := k.options()
	was, _, err := k.retention()
	k.choice = (k.choice + delta + n) % n
	if k.custom() && err == nil {
		// Custom starts from what was showing, so it's an edit of that.
		for i, v := range retentionValues(was) {
			if v == protocol.KeepUnlimited {
				k.counts[i].SetValue("∞")
			} else {
				k.counts[i].SetValue(strconv.Itoa(v))
			}
		}
	}
}

func (k *keepEditor) isDefault() bool { return k.withDefault && k.choice == 0 }
func (k *keepEditor) custom() bool    { return k.choice == k.options()-1 }

// preset is the chosen preset's index, -1 for default or custom.
func (k *keepEditor) preset() int {
	i := k.choice
	if k.withDefault {
		i--
	}
	if i < 0 || i >= len(keepPresets) {
		return -1
	}
	return i
}

// retention is what's chosen; inherit when it's "default". Custom reads
// the counts (which keep their last preset's values until edited).
func (k *keepEditor) retention() (r protocol.Retention, inherit bool, err error) {
	switch {
	case k.isDefault():
		return k.def, true, nil
	case k.preset() >= 0:
		return keepPresets[k.preset()].r, false, nil
	}
	var v [6]int
	for i := range k.counts {
		text := strings.TrimSpace(k.counts[i].Value())
		if text == "∞" {
			v[i] = protocol.KeepUnlimited
			continue
		}
		n, err := strconv.Atoi(text)
		if err != nil || n < 0 {
			return protocol.Retention{}, false, fmt.Errorf("keep %s: a whole number, 0 or more", keepLabels[i])
		}
		v[i] = n
	}
	return protocol.Retention{Last: v[0], Hourly: v[1], Daily: v[2], Weekly: v[3], Monthly: v[4], Yearly: v[5]}, false, nil
}

// text is the choice as the field shows it.
func (k *keepEditor) text() string {
	switch {
	case k.isDefault():
		return "default (" + retentionReach(k.def, false) + ")"
	case k.preset() >= 0:
		return keepPresets[k.preset()].name
	}
	return "custom"
}

// help says what the choice keeps, in words.
func (k *keepEditor) help() string {
	switch {
	case k.isDefault():
		if p := presetFor(k.def); p >= 0 {
			return "Follows the machine's default: " + lowerFirst(keepPresets[p].help)
		}
		return "Follows the machine's default (custom counts; e on the machine's header changes it)."
	case k.preset() >= 0:
		return keepPresets[k.preset()].help
	}
	return "Each line keeps the newest backup of each period, that many periods back; a backup stays if any line keeps it."
}

// countRow is custom count i's value: its input and what it means.
func (k *keepEditor) countRow(i int) string {
	k.counts[i].SetWidth(4)
	return k.counts[i].View() + "  " + styles.MutedStyle().Render(keepMeanings[i])
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// helpLines is how many lines a popup's help text gets, so moving between
// fields with longer and shorter help never changes the popup's height.
const helpLines = 3

// helpBlock is text wrapped at width in exactly helpLines lines (cut
// with … if longer), muted, ending in a newline.
func helpBlock(text string, width int) string {
	lines := strings.Split(lipgloss.NewStyle().Width(width).Render(text), "\n")
	if len(lines) > helpLines {
		lines = lines[:helpLines]
		lines[helpLines-1] = midTrunc(strings.TrimRight(lines[helpLines-1], " ")+"…", width)
	}
	for len(lines) < helpLines {
		lines = append(lines, "")
	}
	return styles.MutedStyle().Render(strings.Join(lines, "\n")) + "\n"
}

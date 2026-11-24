package ui

import (
	"charm-bak/internal/protocol"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// humanBytes renders n in 1024-based units, e.g. "2.1 MB".
func humanBytes(n int64) string {
	// Decimal units, as Finder, S3 and its bill count (Alok, 2026-11-22).
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ago renders the time since t compactly: "now", "20m", "5h", "9d".
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// agoText is ago as a phrase: "just now", "20m ago".
func agoText(now, t time.Time) string {
	if a := ago(now, t); a != "now" {
		return a + " ago"
	}
	return "just now"
}

// took is how long a run lasted, to the second: "18m54s".
func took(r *protocol.Run) string {
	return r.Ended.Sub(r.Started).Round(time.Second).String()
}

// whenNext is a coming time with its day when that isn't today:
// "15:04", "tomorrow 15:04", "Mon 15:04", "Nov 30 15:04".
func whenNext(now, t time.Time) string {
	now, t = now.Local(), t.Local()
	day := func(x time.Time) time.Time { return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, x.Location()) }
	switch days := int(day(t).Sub(day(now)).Hours() / 24); {
	case days <= 0:
		return t.Format("15:04")
	case days == 1:
		return "tomorrow " + t.Format("15:04")
	case days < 7:
		return t.Format("Mon 15:04")
	default:
		return t.Format("Jan 2 15:04")
	}
}

// stamp renders a snapshot time: "Nov 17 22:00", with the year only when
// it isn't the current one.
func stamp(now, t time.Time) string {
	t = t.Local()
	if t.Year() != now.Local().Year() {
		return t.Format("Jan 2 2006")
	}
	return t.Format("Jan 2 15:04")
}

// shortPath abbreviates home to "~".
func shortPath(home, p string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// midTrunc fits s into width cells by cutting from the middle and keeping
// the end: the start says what a thing is, the end tells versions and
// extensions apart ("report-2026…final-v2.pdf"). Plain text only.
func midTrunc(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	tail := (width - 1) / 2
	head := width - 1 - tail
	right := ansi.TruncateLeft(s, ansi.StringWidth(s)-tail, "")
	return ansi.Truncate(s, head, "") + "…" + right
}

// fitRow lays left and right into exactly width cells, right kept whole.
// A plain left is cut in the middle (midTrunc); a styled one, at the end.
func fitRow(left, right string, width int) string {
	rw := lipgloss.Width(right)
	if rw >= width {
		return ansi.Truncate(right, width, "")
	}
	avail := width - rw
	if right != "" {
		avail-- // one space minimum between the two
	}
	if ansi.Strip(left) == left {
		left = midTrunc(left, max(avail, 0))
	} else {
		left = ansi.Truncate(left, max(avail, 0), "…")
	}
	gap := width - lipgloss.Width(left) - rw
	return left + strings.Repeat(" ", max(gap, 0)) + right
}

// padRight pads s with spaces to width cells. It never cuts.
func padRight(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

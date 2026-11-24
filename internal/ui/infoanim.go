package ui

import (
	"math"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/harmonica"

	"charm-bak/internal/ui/styles"
)

// The info popup's height springs to fit each selection as ↑↓ move under
// it, like Quick Look (Alok, 2026-11-24: "animate cause it will be fun").
// Only the height moves: the width is fixed, so text never rewraps, and
// the border slides over content that stays put. Update drives it; render
// only reads infoAnim. Info taller than the screen scrolls (the wheel, or
// shift+↑↓), with the title and the hints staying put.

const infoFPS = 60

// infoTickEvery is the spring's frame time (tests set it to 0, so they
// step the spring without waiting on the clock).
var infoTickEvery = time.Second / infoFPS

// infoSpring is a little under critically damped: quick, with a hint of
// overshoot.
var infoSpring = harmonica.NewSpring(harmonica.FPS(infoFPS), 22, 0.8)

type infoAnim struct {
	pos, vel float64 // displayed content rows (inside the border)
	target   int
	ticking  bool
	gen      int // the tick chain's number: ticks from an older one are dropped
	scroll   int // rows scrolled, when the info is taller than the screen
}

type infoTickMsg struct{ gen int }

func infoTick(gen int) tea.Cmd {
	return tea.Tick(infoTickEvery, func(time.Time) tea.Msg { return infoTickMsg{gen} })
}

// infoRows is the popup's content height for the current selection.
func (a *App) infoRows() int {
	lines, _ := a.infoLayout(popupInnerWidth(a.width))
	return len(lines)
}

// infoMaxRows is the most content rows the popup can show.
func (a *App) infoMaxRows() int { return max(a.bodyHeight()-2, 3) }

// infoShownRows is the content height to draw now, within the screen.
func (a *App) infoShownRows() int {
	n := a.infoAnim.target
	if a.infoAnim.ticking {
		n = int(math.Round(a.infoAnim.pos))
	}
	return max(min(n, a.infoMaxRows()), 3)
}

// infoOpened starts the popup small and springs it open.
func (a *App) infoOpened() tea.Cmd {
	a.infoAnim = infoAnim{pos: 3, target: 3, gen: a.infoAnim.gen + 1}
	return a.infoRetarget()
}

// infoClosed ends the spring; a tick still on its way is dropped.
func (a *App) infoClosed() {
	a.infoAnim.ticking = false
	a.infoAnim.gen++
}

// infoRetarget points the spring at the current selection's height and
// starts ticking if it isn't already.
func (a *App) infoRetarget() tea.Cmd {
	s := &a.infoAnim
	t := a.infoRows()
	if s.ticking {
		s.target = t
		return nil
	}
	if t == s.target {
		return nil
	}
	s.pos = float64(a.infoShownRows())
	s.target, s.ticking = t, true
	return infoTick(s.gen)
}

func (a *App) infoStep(msg infoTickMsg) tea.Cmd {
	s := &a.infoAnim
	if msg.gen != s.gen || !s.ticking {
		return nil
	}
	s.pos, s.vel = infoSpring.Update(s.pos, s.vel, float64(s.target))
	if math.Abs(s.pos-float64(s.target)) < 0.3 && math.Abs(s.vel) < 1 {
		s.pos, s.vel, s.ticking = float64(s.target), 0, false
		return nil
	}
	return infoTick(s.gen)
}

// infoScrollMax is how far the info can scroll: 0 unless it's taller than
// the screen. The title and the hints don't scroll.
func (a *App) infoScrollMax() int {
	return max(a.infoRows()-a.infoMaxRows(), 0)
}

// infoScroll moves a too-tall info by delta rows.
func (a *App) infoScroll(delta int) {
	a.infoAnim.scroll = max(min(a.infoAnim.scroll+delta, a.infoScrollMax()), 0)
}

// fitInfo cuts or pads the laid-out lines (and their owners) to n rows.
// The first line (the title) and the last (the hints) stay; the rows
// between start scroll rows down, and blank rows fill what's left.
func fitInfo(lines []string, owner []int, n, scroll int) ([]string, []int) {
	if len(lines) < 2 || len(lines) == n && scroll == 0 {
		return lines, owner
	}
	last := len(lines) - 1
	room := max(n-2, 0)
	scroll = max(min(scroll, last-1-room), 0)
	outL, outO := []string{lines[0]}, []int{owner[0]}
	for i := 1 + scroll; i < last && len(outL) < 1+room; i++ {
		outL, outO = append(outL, lines[i]), append(outO, owner[i])
	}
	for len(outL) < 1+room {
		outL, outO = append(outL, ""), append(outO, -1)
	}
	return append(outL, lines[last]), append(outO, owner[last])
}

// infoBody is the popup's content as drawn now: what rendering and click
// hit-testing both use. When the info is taller than the screen, the
// hints say how to see the rest.
func (a *App) infoBody() (lines []string, owner []int) {
	lines, owner = a.infoLayout(popupInnerWidth(a.width))
	if over := a.infoScrollMax(); over > 0 {
		scroll := min(a.infoAnim.scroll, over)
		hint := "shift+↑↓ or wheel scrolls"
		switch {
		case scroll == 0:
			hint += " · more below"
		case scroll == over:
			hint += " · more above"
		default:
			hint += " · more above and below"
		}
		lines[len(lines)-1] = styles.MutedStyle().Render(hint + " · esc to close")
	}
	return fitInfo(lines, owner, a.infoShownRows(), a.infoAnim.scroll)
}

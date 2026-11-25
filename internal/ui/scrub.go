package ui

import "strings"

// scrubFrame keeps a frame's colour codes (SGR, ESC [ … m) and drops
// every other escape sequence and control character but the newline.
// The TUI writes nothing else into a frame (a copy to the clipboard goes
// out on its own, as tea.Raw), so anything else came from data: a file,
// source or note name, a remote's reply. Unscrubbed, a name carrying
// OSC 52 would set the clipboard as it scrolled past, OSC 8 make a link,
// CSI 2J clear the screen. lipgloss counts these as zero width already,
// so dropping them leaves the layout as it was measured.
func scrubFrame(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b && i+1 < len(s) && s[i+1] == '[':
			// CSI: parameters and intermediates, then one final byte.
			j := i + 2
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
				j++
			}
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
				if s[j] == 'm' {
					b.WriteString(s[i : j+1])
				}
				i = j + 1
				continue
			}
			i = j
		case c == 0x1b && i+1 < len(s) && (s[i+1] == ']' || s[i+1] == 'P' || s[i+1] == '_' || s[i+1] == '^' || s[i+1] == 'X'):
			// OSC, DCS, APC, PM, SOS: up to BEL or ESC \.
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
				j++
			}
			switch {
			case j >= len(s):
				i = j
			case s[j] == 0x07:
				i = j + 1
			default:
				i = j + 2
			}
		case c == 0x1b:
			i += 2 // ESC and the one byte it introduces
		case c == '\n':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
			i += 2 // C1 controls (U+0080–U+009F), e.g. the one-byte CSI
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isControl(r rune) bool {
	return (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

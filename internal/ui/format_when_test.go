package ui

import (
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

func TestWhenNextAgoTookBytes(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 30, 0, 0, time.Local)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(time.Hour), "15:30"},
		{now.Add(24 * time.Hour), "tomorrow 14:30"},
		{now.Add(3 * 24 * time.Hour), "Wed 14:30"},
		{now.Add(9 * 24 * time.Hour), "Dec 1 14:30"},
	} {
		if got := whenNext(now, c.t); got != c.want {
			t.Errorf("whenNext %v: %q, want %q", c.t, got, c.want)
		}
	}
	if got := agoText(now, now.Add(-10*time.Second)); got != "just now" {
		t.Errorf("agoText: %q", got)
	}
	if got := agoText(now, now.Add(-20*time.Minute)); got != "20m ago" {
		t.Errorf("agoText: %q", got)
	}
	r := &protocol.Run{Started: now, Ended: now.Add(18*time.Minute + 54*time.Second + 300*time.Millisecond)}
	if got := took(r); got != "18m54s" {
		t.Errorf("took: %q", got)
	}
	if got := humanBytes(151_301_916_180); got != "151.3 GB" {
		t.Errorf("humanBytes: %q (decimal units)", got)
	}
}

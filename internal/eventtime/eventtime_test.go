package eventtime

import (
	"testing"
	"time"
)

func TestParseLead(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultLead},
		{"-", DefaultLead},
		{"none", DefaultLead},
		{"garbage", DefaultLead},
		{"30m", 30 * time.Minute},
		{"1h", time.Hour},
		{"1h30m", 90 * time.Minute},
		{"45 min", 45 * time.Minute},
		{"20 minutes", 20 * time.Minute},
		{"2 hours", 2 * time.Hour},
		{"5m", MinLead}, // below the floor is raised to MinLead
		{"1 day", 24 * time.Hour},
	}
	for _, c := range cases {
		if got := ParseLead(c.in); got != c.want {
			t.Errorf("ParseLead(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNextAt(t *testing.T) {
	start := "2026-07-05T15:00:00Z"
	// A 30m lead lands NextAt at 14:30.
	if got := NextAt(start, "30m"); got != "2026-07-05T14:30:00Z" {
		t.Fatalf("NextAt 30m = %q", got)
	}
	// Default when no lead: 30m before.
	if got := NextAt(start, ""); got != "2026-07-05T14:30:00Z" {
		t.Fatalf("NextAt default = %q", got)
	}
	// An event with no set time has no anchor: returned unchanged.
	if got := NextAt("", "30m"); got != "" {
		t.Fatalf("NextAt no-time = %q", got)
	}
	// NextAt is always before EventStart.
	s, _ := time.Parse(time.RFC3339, start)
	n, _ := time.Parse(time.RFC3339, NextAt(start, "1h"))
	if !n.Before(s) {
		t.Fatalf("NextAt %v not before EventStart %v", n, s)
	}
}

func TestShiftPreservesLead(t *testing.T) {
	// Old event: start 15:00, nudge 14:00 -> a 1h lead. Move to 18:00 -> nudge 17:00.
	got := Shift("2026-07-05T15:00:00Z", "2026-07-05T14:00:00Z", "2026-07-05T18:00:00Z")
	if got != "2026-07-05T17:00:00Z" {
		t.Fatalf("Shift preserving 1h lead = %q", got)
	}
	// Legacy row with NextAt == EventStart (no lead) gets the default on move.
	got = Shift("2026-07-05T15:00:00Z", "2026-07-05T15:00:00Z", "2026-07-05T18:00:00Z")
	if got != "2026-07-05T17:30:00Z" {
		t.Fatalf("Shift legacy no-lead = %q, want default 30m before", got)
	}
	// Unparseable newStart is returned unchanged.
	if got := Shift("", "", "not-a-time"); got != "not-a-time" {
		t.Fatalf("Shift bad newStart = %q", got)
	}
}

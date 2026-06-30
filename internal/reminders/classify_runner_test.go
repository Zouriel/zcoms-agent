package reminders

import (
	"testing"
	"time"
)

func TestParseDecision(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.Local) // Tue 10:00
	base := heuristic{}.Classify("the 3pm sync", now)

	out := "KIND: oneoff\nDEADLINE: yes\nEVENT: 2026-06-30T15:00\nPRE: 15\nPOST: 20\nRECUR: none\n"
	d := parseDecision(out, now, base)
	if d.Kind != "oneoff" || !d.DeadlineBound {
		t.Fatalf("kind/deadline: %+v", d)
	}
	if d.EventAt.Hour() != 15 || d.EventAt.Minute() != 0 {
		t.Fatalf("event: %v", d.EventAt)
	}
	if d.PreDelay != 15*time.Minute || d.PostGap != 20*time.Minute {
		t.Fatalf("gaps: pre=%v post=%v", d.PreDelay, d.PostGap)
	}

	// Recurring overlay sets the spec, flips kind, and derives EventAt.
	out = "KIND: recurring\nDEADLINE: no\nEVENT: none\nPRE: 0\nPOST: 10\nRECUR: weekdays 09:00\n"
	d = parseDecision(out, now, base)
	if d.Kind != "recurring" || d.RecurSpec != "weekdays 09:00" {
		t.Fatalf("recurring: %+v", d)
	}
	if d.EventAt.Hour() != 9 || (d.EventAt.Weekday() == time.Saturday || d.EventAt.Weekday() == time.Sunday) {
		t.Fatalf("recurring event: %v (%s)", d.EventAt, d.EventAt.Weekday())
	}

	// Garbage model output leaves the heuristic base untouched.
	d = parseDecision("(the model rambled)", now, base)
	if d.DeadlineBound != base.DeadlineBound || d.PreDelay != base.PreDelay {
		t.Fatalf("garbage should fall back to base: %+v vs %+v", d, base)
	}
}

func TestParseReply(t *testing.T) {
	base := ReplyVerdict{Positive: false}
	if v := parseReply("DONE: yes\nNEXT: none\n", base); !v.Positive {
		t.Fatalf("expected positive: %+v", v)
	}
	v := parseReply("DONE: no\nNEXT: 30\n", base)
	if v.Positive || v.NewGap != 30*time.Minute {
		t.Fatalf("expected negative+30m: %+v", v)
	}
}

func TestNormalizeRecur(t *testing.T) {
	cases := map[string]string{
		"daily 8pm":         "daily 20:00",
		"weekdays 9:00":     "weekdays 09:00",
		"weekday 7am":       "weekdays 07:00",
		"every monday 9am":  "weekly Mon 09:00",
		"weekly fri 17:30":  "weekly Fri 17:30",
	}
	for in, want := range cases {
		if got := normalizeRecur(in); got != want {
			t.Errorf("normalizeRecur(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNextRecur(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.Local) // Tuesday

	// Daily at 09:00 → tomorrow 09:00 (today's already passed).
	if at, ok := nextRecur("daily 09:00", now); !ok || at.Hour() != 9 || at.Day() != 1 {
		t.Fatalf("daily next = %v ok=%v", at, ok)
	}
	// Daily at 18:00 → today 18:00 (still ahead).
	if at, ok := nextRecur("daily 18:00", now); !ok || at.Day() != 30 || at.Hour() != 18 {
		t.Fatalf("daily-today next = %v ok=%v", at, ok)
	}
	// Weekly Mon → next Monday (Jul 6 2026).
	at, ok := nextRecur("weekly Mon 09:00", now)
	if !ok || at.Weekday() != time.Monday {
		t.Fatalf("weekly next = %v (%s) ok=%v", at, at.Weekday(), ok)
	}
}

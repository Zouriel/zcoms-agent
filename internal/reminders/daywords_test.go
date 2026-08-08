package reminders

import (
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// The lookup has to understand the words people (and the chat agent relaying
// them) actually use. "events today" used to be a parse error, so the agent
// reported that it couldn't see the schedule rather than reading out the day.
func TestParseAnchorDayWords(t *testing.T) {
	// A Saturday, so the weekday cases below have an unambiguous answer.
	now := time.Date(2026, 8, 8, 14, 30, 0, 0, time.Local)

	cases := []struct {
		in   string
		want string // the local day the anchor should land on
	}{
		{"today", "2026-08-08"},
		{"Today", "2026-08-08"},
		{"tonight", "2026-08-08"},
		{"this evening", "2026-08-08"},
		{"tomorrow", "2026-08-09"},
		{"Tomorrow morning", "2026-08-09"},
		{"yesterday", "2026-08-07"},
		{"day after tomorrow", "2026-08-10"},
		{"monday", "2026-08-10"}, // the coming Monday
		{"next friday", "2026-08-14"},
		{"saturday", "2026-08-08"}, // today is Saturday
	}
	for _, c := range cases {
		at, hasTime, err := parseAnchor(c.in, now)
		if err != nil {
			t.Errorf("parseAnchor(%q) errored: %v", c.in, err)
			continue
		}
		if got := at.Format("2006-01-02"); got != c.want {
			t.Errorf("parseAnchor(%q) = %s, want %s", c.in, got, c.want)
		}
		if hasTime {
			t.Errorf("parseAnchor(%q) reported a clock time; a day word means the whole day", c.in)
		}
	}
}

// The forms that already worked must keep working, and a day word must not
// swallow something that isn't one.
func TestParseAnchorStillHandlesDatesAndTimes(t *testing.T) {
	now := time.Date(2026, 8, 8, 14, 30, 0, 0, time.Local)

	if at, hasTime, err := parseAnchor("2026-08-31", now); err != nil || hasTime || at.Format("2006-01-02") != "2026-08-31" {
		t.Fatalf("bare date broke: %v %v %v", at, hasTime, err)
	}
	if at, hasTime, err := parseAnchor("2026-08-31 09:00", now); err != nil || !hasTime || at.Hour() != 9 {
		t.Fatalf("date+time broke: %v %v %v", at, hasTime, err)
	}
	if _, _, err := parseAnchor("someday", now); err == nil {
		t.Fatal("a word that isn't a day should still be an error")
	}
}

// A day-word lookup must actually find that day's events — the whole point of
// the fix.
func TestEventsReportFindsTodayByDayWord(t *testing.T) {
	now := time.Date(2026, 8, 8, 14, 30, 0, 0, time.Local)
	start := time.Date(2026, 8, 8, 18, 0, 0, 0, time.Local)
	rows := []store.Reminder{{
		ID: 7, Task: "class", State: store.ReminderActive,
		EventStart: start.UTC().Format(time.RFC3339),
	}}

	at, hasTime, err := parseAnchor("today", now)
	if err != nil {
		t.Fatalf("parseAnchor: %v", err)
	}
	report := eventsReport(rows, at, hasTime)
	if !strings.Contains(report, "class") {
		t.Fatalf("today's event missing from the report:\n%s", report)
	}
}

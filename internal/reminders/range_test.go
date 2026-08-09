package reminders

import (
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// The words the MCP tool advertises ("today", "tomorrow", "this week") all have to
// resolve. "this week" used to reach parseAnchor, which only understands single
// moments, so the schedule lookup came back as a parse error.
func TestParseRangeWords(t *testing.T) {
	// A Wednesday, so "this week" has days on both sides of it.
	now := time.Date(2026, 8, 12, 14, 0, 0, 0, time.Local)

	cases := []struct {
		in       string
		wantLo   string // local day the window opens on
		wantHi   string // local day it closes on
		inHeader string
	}{
		{"this week", "2026-08-12", "2026-08-16", "this week"},
		{"rest of the week", "2026-08-12", "2026-08-16", "this week"},
		{"next week", "2026-08-17", "2026-08-23", "next week"},
		{"last week", "2026-08-03", "2026-08-09", "last week"},
		{"this weekend", "2026-08-15", "2026-08-16", "this weekend"},
		{"this month", "2026-08-12", "2026-08-31", "this month"},
		{"next month", "2026-09-01", "2026-09-30", "next month"},
		{"next 3 days", "2026-08-12", "2026-08-14", "next 3 day(s)"},
		{"the next 2 weeks", "2026-08-12", "2026-08-25", "next 2 week(s)"},
	}

	for _, c := range cases {
		lo, hi, header, ok := parseRange(c.in, now)
		if !ok {
			t.Errorf("%q: not recognised as a range", c.in)
			continue
		}
		if got := lo.Format("2006-01-02"); got != c.wantLo {
			t.Errorf("%q: opens %s, want %s", c.in, got, c.wantLo)
		}
		if got := hi.Format("2006-01-02"); got != c.wantHi {
			t.Errorf("%q: closes %s, want %s", c.in, got, c.wantHi)
		}
		if !strings.Contains(header, c.inHeader) {
			t.Errorf("%q: header %q missing %q", c.in, header, c.inHeader)
		}
		if !hi.After(lo) {
			t.Errorf("%q: empty window %v..%v", c.in, lo, hi)
		}
	}
}

// On the last day of the week, "this week" cannot mean that one day: the week
// being asked about is the one starting tomorrow.
func TestThisWeekOnSundayLooksAhead(t *testing.T) {
	sunday := time.Date(2026, 8, 16, 10, 0, 0, 0, time.Local)
	lo, hi, _, ok := parseRange("this week", sunday)
	if !ok {
		t.Fatal("this week should be a range")
	}
	if got := lo.Format("2006-01-02"); got != "2026-08-16" {
		t.Fatalf("opens %s, want today", got)
	}
	if got := hi.Format("2006-01-02"); got != "2026-08-23" {
		t.Fatalf("closes %s, want the end of the coming week", got)
	}

	// A Saturday still has a day left in its own week, so it is not rolled forward.
	saturday := time.Date(2026, 8, 15, 10, 0, 0, 0, time.Local)
	_, satHi, _, _ := parseRange("this week", saturday)
	if got := satHi.Format("2006-01-02"); got != "2026-08-16" {
		t.Fatalf("saturday closes %s, want 2026-08-16", got)
	}
}

// Single days stay with parseAnchor, which knows how to render one day.
func TestParseRangeLeavesSingleDays(t *testing.T) {
	now := time.Date(2026, 8, 12, 14, 0, 0, 0, time.Local)
	for _, s := range []string{"today", "tomorrow", "friday", "2026-08-14", "2026-08-14 15:00", "gibberish"} {
		if _, _, _, ok := parseRange(s, now); ok {
			t.Errorf("%q should not be treated as a multi-day range", s)
		}
	}
}

// A week's worth of events is only readable grouped under its days.
func TestSpanReportGroupsByDay(t *testing.T) {
	loc := time.Local
	lo := time.Date(2026, 8, 12, 0, 0, 0, 0, loc)
	hi := time.Date(2026, 8, 16, 23, 59, 59, 0, loc)
	at := func(d, h int) string { return rfc(time.Date(2026, 8, d, h, 0, 0, 0, loc)) }

	all := []store.Reminder{
		{ID: 1, Task: "dentist", State: store.ReminderActive, EventStart: at(13, 9)},
		{ID: 2, Task: "school run", State: store.ReminderActive, EventStart: at(13, 15)},
		{ID: 3, Task: "flight", State: store.ReminderActive, EventStart: at(15, 6)},
		{ID: 4, Task: "way out", State: store.ReminderActive, EventStart: at(30, 6)},
	}

	out := spanReport(all, lo, hi, "Events this week:", true)
	for _, want := range []string{"Thu 13 Aug", "Sat 15 Aug", "dentist", "school run", "flight"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "way out") {
		t.Fatalf("event outside the window leaked:\n%s", out)
	}
	// Each day heading appears once, however many events sit under it.
	if strings.Count(out, "Thu 13 Aug") != 1 {
		t.Fatalf("day heading repeated:\n%s", out)
	}
}

func TestEventsAroundAcceptsRanges(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	now := time.Now()
	if _, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "midweek thing", State: store.ReminderActive,
		EventStart: rfc(now.Add(24 * time.Hour)),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, when := range []string{"today", "tomorrow", "this week", "next week", "this weekend", "next 7 days"} {
		if _, err := d.EventsAround(when, now); err != nil {
			t.Errorf("events %q: %v", when, err)
		}
	}
	if _, err := d.EventsAround("gibberish", now); err == nil {
		t.Error("gibberish should still error")
	}
}

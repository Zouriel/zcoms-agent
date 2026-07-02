package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestParseAnchor(t *testing.T) {
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.Local)

	// A past absolute date-time is accepted (unlike the scheduler parser).
	got, hasTime, err := parseAnchor("2020-01-02 15:00", now)
	if err != nil || !hasTime || got.Year() != 2020 {
		t.Fatalf("past datetime: %v %v %v", got, hasTime, err)
	}
	// Date only reports hasTime=false (whole-day window).
	if _, ht, err := parseAnchor("2026-07-04", now); err != nil || ht {
		t.Fatalf("date-only should be hasTime=false: %v %v", ht, err)
	}
	if _, _, err := parseAnchor("gibberish", now); err == nil {
		t.Fatal("gibberish should error")
	}
}

func TestEventsReportWindow(t *testing.T) {
	loc := time.Local
	anchor := time.Date(2026, 7, 4, 15, 0, 0, 0, loc) // 3:00 PM
	at := func(h, m int) string { return rfc(time.Date(2026, 7, 4, h, m, 0, 0, loc)) }

	all := []store.Reminder{
		{ID: 1, Task: "coffee with Sam", State: store.ReminderActive, EventStart: at(14, 0), OtherParty: "Sam"}, // in window (2pm)
		{ID: 2, Task: "dentist", State: store.ReminderActive, EventStart: at(15, 0), EventEnd: at(16, 0)},       // in window (3-4pm)
		{ID: 3, Task: "far away", State: store.ReminderActive, EventStart: at(20, 0)},                           // 8pm, outside ±2h
		{ID: 4, Task: "cancelled one", State: store.ReminderCancelled, EventStart: at(15, 30)},                  // skipped
		{ID: 5, Task: "no time", State: store.ReminderActive},                                                   // no time, skipped
		{ID: 6, Task: "next_at only", State: store.ReminderActive, NextAt: at(16, 30)},                          // in window via next_at
	}

	out := eventsReport(all, anchor, true)
	if !strings.Contains(out, "coffee with Sam (with Sam)") || !strings.Contains(out, "dentist") || !strings.Contains(out, "next_at only") {
		t.Fatalf("expected in-window events, got:\n%s", out)
	}
	if strings.Contains(out, "far away") || strings.Contains(out, "cancelled one") || strings.Contains(out, "no time") {
		t.Fatalf("out-of-window / cancelled / untimed leaked:\n%s", out)
	}
	// dentist has a range, so it shows a start–end label.
	if !strings.Contains(out, "3:00 PM–4:00 PM") {
		t.Fatalf("range label missing:\n%s", out)
	}
	// Ordered by start: coffee (2pm) before dentist (3pm).
	if strings.Index(out, "coffee") > strings.Index(out, "dentist") {
		t.Fatalf("not sorted by time:\n%s", out)
	}

	// Nothing in window.
	empty := eventsReport(all, time.Date(2026, 7, 4, 6, 0, 0, 0, loc), true)
	if !strings.Contains(empty, "nothing on the calendar") {
		t.Fatalf("expected empty window message, got:\n%s", empty)
	}
}

func TestEventsReportWholeDay(t *testing.T) {
	loc := time.Local
	day := time.Date(2026, 7, 4, 0, 0, 0, 0, loc)
	at := func(h int) string { return rfc(time.Date(2026, 7, 4, h, 0, 0, 0, loc)) }
	other := rfc(time.Date(2026, 7, 5, 9, 0, 0, 0, loc))

	all := []store.Reminder{
		{ID: 1, Task: "early", State: store.ReminderActive, EventStart: at(7)},
		{ID: 2, Task: "late", State: store.ReminderActive, EventStart: at(22)},
		{ID: 3, Task: "next day", State: store.ReminderActive, EventStart: other},
	}
	out := eventsReport(all, day, false) // hasTime=false → whole day
	if !strings.Contains(out, "Events on") || !strings.Contains(out, "early") || !strings.Contains(out, "late") {
		t.Fatalf("whole-day should include both same-day events:\n%s", out)
	}
	if strings.Contains(out, "next day") {
		t.Fatalf("next-day event leaked into today:\n%s", out)
	}
}

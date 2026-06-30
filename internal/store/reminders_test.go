package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTmpStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReminderCRUD(t *testing.T) {
	s := openTmpStore(t)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	r, err := s.CreateReminder(Reminder{
		FromAddr: "telegram|@me", FromName: "you",
		RecipientTransport: "telegram", RecipientAddr: "12345", RecipientName: "you",
		Task: "buy a rose", State: ReminderActive, NextAt: past,
	})
	if err != nil || r.ID == 0 {
		t.Fatalf("create: %v id=%d", err, r.ID)
	}

	// Due scan picks up a past next_at.
	if due, _ := s.DueReminders(time.Now().UTC().Format(time.RFC3339)); len(due) != 1 || due[0].ID != r.ID {
		t.Fatalf("DueReminders = %v", due)
	}

	// Agent updates carry-over + next time + run count.
	r.CarryOver = "nudged once, awaiting reply"
	r.NextAt = future
	r.Runs = 1
	if err := s.UpdateReminder(r); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, _ := s.GetReminder(r.ID)
	if got.CarryOver != "nudged once, awaiting reply" || got.Runs != 1 {
		t.Fatalf("carry/runs not persisted: %+v", got)
	}
	if due, _ := s.DueReminders(time.Now().UTC().Format(time.RFC3339)); len(due) != 0 {
		t.Fatalf("future reminder reported due: %v", due)
	}

	// Events timeline.
	s.AddReminderEvent(r.ID, "run", "")
	s.AddReminderEvent(r.ID, "send", "hey")
	if evs, _ := s.ListReminderEvents(r.ID); len(evs) != 2 {
		t.Fatalf("events = %v", evs)
	}

	// Cancel removes it from active + due.
	if err := s.CancelReminder(r.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if active, _ := s.ActiveReminders(); len(active) != 0 {
		t.Fatalf("cancelled still active: %v", active)
	}
}

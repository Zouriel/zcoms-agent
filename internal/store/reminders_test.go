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
		RequesterAddr:   "telegram|@me",
		TargetTransport: "telegram", TargetAddr: "12345", TargetName: "you",
		TaskText: "buy a rose", Kind: "oneoff",
		State: ReminderScheduled, NextAt: past,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == 0 {
		t.Fatal("no id assigned")
	}

	// Due scan picks up a past next_at.
	due, err := s.DueReminders(time.Now().UTC().Format(time.RFC3339))
	if err != nil || len(due) != 1 || due[0].ID != r.ID {
		t.Fatalf("DueReminders = %v, %v", due, err)
	}

	// A future next_at is not due.
	r.NextAt = future
	if err := s.UpdateReminder(r); err != nil {
		t.Fatalf("update: %v", err)
	}
	due, _ = s.DueReminders(time.Now().UTC().Format(time.RFC3339))
	if len(due) != 0 {
		t.Fatalf("future reminder reported due: %v", due)
	}

	// Open-for-target correlation (the reply matcher's lookup).
	r.State = ReminderAwaiting
	if err := s.UpdateReminder(r); err != nil {
		t.Fatalf("update awaiting: %v", err)
	}
	got, ok, err := s.OpenReminderForTarget("telegram", "12345")
	if err != nil || !ok || got.ID != r.ID {
		t.Fatalf("OpenReminderForTarget = %v,%v,%v", got.ID, ok, err)
	}
	if _, ok, _ := s.OpenReminderForTarget("telegram", "99999"); ok {
		t.Fatal("matched a target with no reminder")
	}

	// Cancel removes it from active + due + open scans.
	if err := s.CancelReminder(r.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	active, _ := s.ActiveReminders()
	if len(active) != 0 {
		t.Fatalf("cancelled reminder still active: %v", active)
	}
	if !IsTerminalReminder(ReminderCancelled) || IsTerminalReminder(ReminderScheduled) {
		t.Fatal("IsTerminalReminder wrong")
	}
}

package reminders

import (
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// A reminder list with no times in it cannot be acted on: there is no way to tell
// one due tonight from one that went past weeks ago.
func TestListShowsTimes(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	now := time.Now()

	if _, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		RecipientName: "you", Task: "collect the report", State: store.ReminderActive,
		NextAt: rfc(now.Add(-49 * time.Hour)),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		RecipientName: "Sara", Task: "send the invoice", State: store.ReminderActive,
		NextAt: rfc(now.Add(3 * time.Hour)),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	out := d.listReply(Requester{Owner: true}, false)
	if !strings.Contains(out, "due ") {
		t.Fatalf("no due time in list:\n%s", out)
	}
	if !strings.Contains(out, "overdue by 2d") {
		t.Fatalf("a two-day-late reminder should be flagged overdue:\n%s", out)
	}
	if !strings.Contains(out, "set ") {
		t.Fatalf("no set-at time in list:\n%s", out)
	}
	// The future one is not overdue, so only one line carries the marker.
	if strings.Count(out, "overdue") != 1 {
		t.Fatalf("expected exactly one overdue marker:\n%s", out)
	}
}

// `remind list` is the working list; `remind list all` is the one that answers
// "what did I set and what happened to it".
func TestListAllIncludesFinished(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	now := time.Now()

	_, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "finished thing", State: store.ReminderDone, NextAt: rfc(now.Add(-time.Hour)),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "live thing", State: store.ReminderActive, NextAt: rfc(now.Add(time.Hour)),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	owner := Requester{Owner: true}

	if out := d.listReply(owner, false); strings.Contains(out, "finished thing") {
		t.Fatalf("a finished reminder leaked into the working list:\n%s", out)
	}
	out := d.listReply(owner, true)
	if !strings.Contains(out, "finished thing") || !strings.Contains(out, "live thing") {
		t.Fatalf("list all should show both:\n%s", out)
	}
	if !strings.Contains(out, "["+store.ReminderDone+"]") {
		t.Fatalf("finished reminders should be labelled with their state:\n%s", out)
	}
	if out := d.HandleCommand(owner, "remind list all"); !strings.Contains(out, "finished thing") {
		t.Fatalf("`remind list all` should route to the full list:\n%s", out)
	}

}

func TestEditTaskAndNote(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	r, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "old task", State: store.ReminderActive, NextAt: rfc(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	owner := Requester{Owner: true}

	if out := d.editReply(owner, []string{itoa(r.ID), "task", "pick up the", "report"}); !strings.Contains(out, "updated") {
		t.Fatalf("edit task: %s", out)
	}
	if got := reload(t, st, r.ID).Task; got != "pick up the report" {
		t.Fatalf("task not saved, got %q", got)
	}

	if out := d.editReply(owner, []string{itoa(r.ID), "note", "he is away until Friday"}); !strings.Contains(out, "updated") {
		t.Fatalf("edit note: %s", out)
	}
	if got := reload(t, st, r.ID).CarryOver; got != "he is away until Friday" {
		t.Fatalf("note not saved, got %q", got)
	}
}

// A day word carries no clock time, so an edit to "tomorrow" has to keep the time
// of day the reminder already had rather than dropping it to midnight.
func TestEditWhenKeepsTimeOfDay(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	at := time.Date(2026, 8, 9, 17, 30, 0, 0, time.Local)
	r, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "call the clinic", State: store.ReminderActive, NextAt: rfc(at),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if out := d.editReply(Requester{Owner: true}, []string{itoa(r.ID), "when", "tomorrow"}); !strings.Contains(out, "updated") {
		t.Fatalf("edit when: %s", out)
	}
	got := parseInstant(reload(t, st, r.ID).NextAt, time.Local)
	if got.Hour() != 17 || got.Minute() != 30 {
		t.Fatalf("time of day lost: %v", got)
	}
	if !got.After(time.Now()) {
		t.Fatalf("edited reminder should be in the future, got %v", got)
	}
}

// Moving an event-backed reminder moves the event and keeps the lead, so the
// nudge does not collapse onto the event itself.
func TestEditWhenPreservesLead(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	start := time.Now().Add(48 * time.Hour).Truncate(time.Minute)
	r, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "dentist", State: store.ReminderActive,
		EventStart: rfc(start), NextAt: rfc(start.Add(-time.Hour)),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newStart := start.AddDate(0, 0, 3)
	when := newStart.Format("2006-01-02 15:04")
	if out := d.editReply(Requester{Owner: true}, []string{itoa(r.ID), "when", when}); !strings.Contains(out, "updated") {
		t.Fatalf("edit when: %s", out)
	}

	got := reload(t, st, r.ID)
	ev, next := parseInstant(got.EventStart, time.Local), parseInstant(got.NextAt, time.Local)
	if ev.IsZero() || next.IsZero() {
		t.Fatalf("times missing after edit: %+v", got)
	}
	if lead := ev.Sub(next); lead != time.Hour {
		t.Fatalf("lead not preserved, got %v", lead)
	}
}

// The §6 gate applies to edits exactly as it does to cancels: a stranger cannot
// rewrite someone else's reminder.
func TestEditRefusesStranger(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	r, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "private thing", State: store.ReminderActive, NextAt: rfc(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	stranger := Requester{Transport: "telegram", Handle: "someone", Address: "77"}
	if out := d.editReply(stranger, []string{itoa(r.ID), "task", "mine now"}); !strings.Contains(out, "isn't yours") {
		t.Fatalf("stranger should be refused, got: %s", out)
	}
	if got := reload(t, st, r.ID).Task; got != "private thing" {
		t.Fatalf("task changed despite refusal: %q", got)
	}
}

func TestEditUsage(t *testing.T) {
	d, _, _ := newTestComp(t, nil)
	owner := Requester{Owner: true}
	if out := d.editReply(owner, []string{"1"}); !strings.Contains(out, "Usage:") {
		t.Fatalf("short form should show usage, got: %s", out)
	}
	if out := d.editReply(owner, []string{"nope", "task", "x"}); !strings.Contains(out, "bad reminder id") {
		t.Fatalf("bad id should say so, got: %s", out)
	}
	if out := d.editReply(owner, []string{"4242", "task", "x"}); !strings.Contains(out, "No reminder") {
		t.Fatalf("missing reminder should say so, got: %s", out)
	}
}

// remind delete/remove are the words people reach for; they have to land on cancel
// rather than falling through and creating a reminder called "delete 29".
func TestDeleteAliasesCancel(t *testing.T) {
	d, _, st := newTestComp(t, nil)
	r, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|owner", RecipientTransport: "telegram", RecipientAddr: "1",
		Task: "temporary", State: store.ReminderActive, NextAt: rfc(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out := d.HandleCommand(Requester{Owner: true}, "remind delete "+itoa(r.ID)); !strings.Contains(out, "cancelled") {
		t.Fatalf("delete should cancel, got: %s", out)
	}
	if got := reload(t, st, r.ID).State; got != store.ReminderCancelled {
		t.Fatalf("state is %q, want cancelled", got)
	}
}

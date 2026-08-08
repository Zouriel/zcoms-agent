package lineup

import (
	"testing"
	"time"
)

// testComp reuses the package harness; the agent turn is never driven here since
// applyAction is called directly.
func testComp(t *testing.T) *Comp {
	t.Helper()
	d, _, _ := newTestComp(t, nil)
	return d
}

// A request to remind somebody else must leave the owner's calendar. Filed here,
// it became an owner event whose task named another person, and the reminder loop
// then greeted the owner as that person ("Morning Raani…" to the owner).
func TestAddRoutesThirdPartyReminder(t *testing.T) {
	d := testComp(t)
	var gotWho, gotTask string
	d.SetRemindOther(func(who, task string) string {
		gotWho, gotTask = who, task
		return "✅ Reminder #9 set. I'll remind Raani to take her father's report."
	})

	d.applyAction(map[string]string{
		"action": "add",
		"task":   "Remind Raani to take her father's report",
	}, "telegram", "42", time.Now())

	if gotWho != "Raani" || gotTask != "take her father's report" {
		t.Fatalf("delegate got (%q, %q), want (Raani, take her father's report)", gotWho, gotTask)
	}
	rows, err := d.store.ActiveReminders()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.RecipientName == "you" && r.Task == "Remind Raani to take her father's report" {
			t.Fatal("the third-party request was still filed as one of the owner's own events")
		}
	}
}

// The owner's own events are untouched by the guard.
func TestAddKeepsOwnEvent(t *testing.T) {
	d := testComp(t)
	d.SetRemindOther(func(who, task string) string {
		t.Fatalf("own event was routed away as a third-party reminder: (%q, %q)", who, task)
		return ""
	})

	d.applyAction(map[string]string{
		"action": "add",
		"task":   "Dentist appointment",
		"start":  "+2h",
	}, "telegram", "42", time.Now())

	rows, err := d.store.ActiveReminders()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Task != "Dentist appointment" {
		t.Fatalf("own event not stored: %+v", rows)
	}
}

// With no delegate wired (headless/tests), the old behaviour stands rather than
// dropping what the owner said.
func TestAddWithoutDelegateStillStores(t *testing.T) {
	d := testComp(t)
	d.applyAction(map[string]string{
		"action": "add",
		"task":   "Remind Raani to take her father's report",
	}, "telegram", "42", time.Now())

	rows, err := d.store.ActiveReminders()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the request to be kept, got %+v", rows)
	}
}

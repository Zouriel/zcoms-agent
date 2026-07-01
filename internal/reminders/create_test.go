package reminders

import (
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// §6 trust at creation: owner may remind anyone in contacts; a non-owner
// allow-listed requester may only target other allow-listed people; self always.
func TestTrustGate(t *testing.T) {
	d, fc, st := newTestComp(t, (&fakeTurn{}).run)
	fc.contacts = []client.Contact{{ID: 7, Name: "Sara", Telegram: "@sara"}}

	owner := Requester{Transport: "telegram", Handle: "@owner", Address: "1", Name: "you", Owner: true}
	nonOwner := Requester{Transport: "telegram", Handle: "@ali", Address: "2", Owner: false}

	if got := d.createReply(nonOwner, "remind Sara to send the invoice"); !strings.Contains(got, "allow-listed") {
		t.Fatalf("expected trust rejection, got %q", got)
	}
	if rs, _ := st.ListReminders(); len(rs) != 0 {
		t.Fatalf("rejected reminder should not persist; got %d", len(rs))
	}

	if got := d.createReply(owner, "remind Sara to send the invoice"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("owner create failed: %q", got)
	}
	if _, err := st.CreateAllow(store.Owner, store.AllowEntry{Platform: "telegram", Handle: "@sara", MaxRole: "read"}); err != nil {
		t.Fatalf("seed allow: %v", err)
	}
	if got := d.createReply(nonOwner, "remind Sara to call back"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("non-owner allow-listed create failed: %q", got)
	}
	if got := d.createReply(nonOwner, "remind me to stretch"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("self create failed: %q", got)
	}
}

// A created reminder lands active, due now (so the first run plans the timing),
// with the recipient + task set.
func TestCreatePersistsActiveDueNow(t *testing.T) {
	d, _, st := newTestComp(t, (&fakeTurn{}).run)
	owner := Requester{Transport: "telegram", Handle: "@owner", Address: "55", Name: "you", Owner: true}

	if got := d.createReply(owner, "remind me to water the plants"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("create: %q", got)
	}
	rs, _ := st.ActiveReminders()
	if len(rs) != 1 {
		t.Fatalf("want 1 active reminder, got %d", len(rs))
	}
	r := rs[0]
	if r.Task != "water the plants" || r.RecipientAddr != "55" || r.State != store.ReminderActive {
		t.Fatalf("row: %+v", r)
	}
	at, err := time.Parse(time.RFC3339, r.NextAt)
	if err != nil || at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("first run should be due ~now: %q", r.NextAt)
	}
}

// Registration runs a planning turn so lead time is decided up front and the
// first run is scheduled ahead (not at creation).
func TestRegistrationPlansLeadTime(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"WHO: me\nTASK: get ready for class at 6",
		"NEXT: +45m\nNOTE: nudge to get ready, ~45m before class",
	}}
	d, _, st := newTestComp(t, ft.run)
	owner := Requester{Transport: "telegram", Handle: "@owner", Address: "7", Name: "you", Owner: true}

	reply := d.createReply(owner, "remind me to get ready for class at 6")
	if !strings.HasPrefix(reply, "✅") || !strings.Contains(reply, "starting") {
		t.Fatalf("confirmation should state the planned time: %q", reply)
	}
	rs, _ := st.ActiveReminders()
	if len(rs) != 1 {
		t.Fatalf("want 1 reminder, got %d", len(rs))
	}
	r := rs[0]
	at, err := time.Parse(time.RFC3339, r.NextAt)
	if err != nil {
		t.Fatalf("next_at: %v", err)
	}
	if d := time.Until(at); d < 40*time.Minute || d > 50*time.Minute {
		t.Fatalf("first run not ~45m out: %v", d)
	}
	if r.CarryOver == "" {
		t.Fatal("carry_over from the plan not stored")
	}
}

// The agent interprets a natural-language request, so an incidental "to" in the
// task ("has to come") is no longer mistaken for the who/task separator — the
// exact phrasing that used to fail with `named "Zouriel that he has"`.
func TestInterpretsNaturalLanguage(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"WHO: Zouriel\nTASK: come pick me up at 11:45",
		"NEXT: NOW\nNOTE: pickup at 11:45",
	}}
	d, fc, st := newTestComp(t, ft.run)
	fc.contacts = []client.Contact{{ID: 3, Name: "Zouriel", Telegram: "@zouriel"}}
	owner := Requester{Transport: "telegram", Handle: "@owner", Address: "1", Name: "Kuri", Owner: true}

	got := d.createReply(owner, "Remind Zouriel that he has to come pick me up at 1145")
	if !strings.HasPrefix(got, "✅") {
		t.Fatalf("expected success, got %q", got)
	}
	rs, _ := st.ActiveReminders()
	if len(rs) != 1 {
		t.Fatalf("want 1 reminder, got %d", len(rs))
	}
	if rs[0].RecipientName != "Zouriel" || rs[0].Task != "come pick me up at 11:45" {
		t.Fatalf("row: %+v", rs[0])
	}
}

// With no agent backend (turn is nil), registration still works via the regex
// fallback, so the command never hard-depends on the model being reachable.
func TestInterpretFallsBackWithoutBackend(t *testing.T) {
	d, fc, st := newTestComp(t, nil)
	fc.contacts = []client.Contact{{ID: 3, Name: "Zouriel", Telegram: "@zouriel"}}
	owner := Requester{Transport: "telegram", Handle: "@owner", Address: "1", Name: "Kuri", Owner: true}

	if got := d.createReply(owner, "remind Zouriel to call back"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("fallback create failed: %q", got)
	}
	rs, _ := st.ActiveReminders()
	if len(rs) != 1 || rs[0].Task != "call back" {
		t.Fatalf("rows: %+v", rs)
	}
}

// settings round-trip live (no restart).
func TestSettingsLive(t *testing.T) {
	d, _, _ := newTestComp(t, (&fakeTurn{}).run)
	owner := Requester{Owner: true}
	if got := d.settingsReply(Requester{Owner: false}, []string{"max_runs", "5"}); !strings.Contains(got, "owner") {
		t.Fatalf("non-owner should be rejected: %q", got)
	}
	if got := d.settingsReply(owner, []string{"reply_wait_mins", "3"}); !strings.HasPrefix(got, "✅") {
		t.Fatalf("set failed: %q", got)
	}
	if d.cfg().ReplyWaitMins != 3 {
		t.Fatalf("live read = %d, want 3", d.cfg().ReplyWaitMins)
	}
	if got := d.settingsReply(owner, []string{"nope", "1"}); !strings.Contains(got, "unknown") {
		t.Fatalf("bad key should be rejected: %q", got)
	}
}

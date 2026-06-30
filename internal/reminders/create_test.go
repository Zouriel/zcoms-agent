package reminders

import (
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// TestTrustGate exercises §6 at creation: owner may remind anyone in contacts; a
// non-owner allow-listed requester may only target other allow-listed people; and
// a self reminder is always allowed.
func TestTrustGate(t *testing.T) {
	clf := &fakeClassifier{dec: Decision{Kind: "oneoff", PreDelay: time.Hour, PostGap: 15 * time.Minute}}
	d, _, st := newTestComp(t, clf)
	d.client.(*fakeClient).contacts = []client.Contact{{ID: 7, Name: "Sara", Telegram: "@sara"}}

	owner := Requester{Transport: "telegram", Handle: "@me", Address: "1", Owner: true}
	nonOwner := Requester{Transport: "telegram", Handle: "@ali", Address: "2", Owner: false}

	// Non-owner targeting a non-allow-listed contact → rejected, nothing persisted.
	got := d.createReply(nonOwner, "remind Sara to send the invoice")
	if !strings.Contains(got, "allow-listed") {
		t.Fatalf("expected trust rejection, got %q", got)
	}
	if rs, _ := st.ListReminders(); len(rs) != 0 {
		t.Fatalf("rejected reminder should not persist; got %d rows", len(rs))
	}

	// Owner may remind anyone in contacts.
	if got := d.createReply(owner, "remind Sara to send the invoice"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("owner create failed: %q", got)
	}

	// Allow-list Sara → the non-owner requester is now permitted.
	if _, err := st.CreateAllow(store.Owner, store.AllowEntry{Platform: "telegram", Handle: "@sara", MaxRole: "read"}); err != nil {
		t.Fatalf("seed allow: %v", err)
	}
	if got := d.createReply(nonOwner, "remind Sara to call back"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("non-owner allow-listed create failed: %q", got)
	}

	// A self reminder is always allowed.
	if got := d.createReply(nonOwner, "remind me to stretch"); !strings.HasPrefix(got, "✅") {
		t.Fatalf("self create failed: %q", got)
	}
}

// TestCreatePersistsAndSchedules checks a created reminder lands in 'scheduled'
// with a future next_at and the inferred fields.
func TestCreatePersistsAndSchedules(t *testing.T) {
	clf := &fakeClassifier{dec: Decision{Kind: "oneoff", PreDelay: time.Hour, PostGap: 15 * time.Minute}}
	d, _, st := newTestComp(t, clf)
	owner := Requester{Transport: "telegram", Handle: "@me", Address: "1", Owner: true}

	reply := d.createReply(owner, "remind me to water the plants")
	if !strings.HasPrefix(reply, "✅") {
		t.Fatalf("create: %q", reply)
	}
	rs, _ := st.ListReminders()
	if len(rs) != 1 {
		t.Fatalf("want 1 reminder, got %d", len(rs))
	}
	r := rs[0]
	if r.State != store.ReminderScheduled || r.TaskText != "water the plants" {
		t.Fatalf("row: %+v", r)
	}
	at, err := time.Parse(time.RFC3339, r.NextAt)
	if err != nil || !at.After(time.Now()) {
		t.Fatalf("next_at not future: %q (%v)", r.NextAt, err)
	}
}

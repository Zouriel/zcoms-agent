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

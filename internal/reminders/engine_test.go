package reminders

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// fakeClient records outbound sends so the engine's behavior is observable.
type fakeClient struct {
	sent     []string // "transport|addr|text"
	marks    int
	contacts []client.Contact
}

func (f *fakeClient) SendOn(transport, to, text string) (client.Response, error) {
	f.sent = append(f.sent, transport+"|"+to+"|"+text)
	return client.Response{}, nil
}
func (f *fakeClient) MarkReadOn(string, string, []string) error { f.marks++; return nil }
func (f *fakeClient) Resolve(string) (int64, error)             { return 1, nil }
func (f *fakeClient) ResolveContact(who string) ([]client.Contact, error) {
	var out []client.Contact
	for _, c := range f.contacts {
		if strings.HasPrefix(strings.ToLower(c.Name), strings.ToLower(who)) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeClient) lastText() string {
	if len(f.sent) == 0 {
		return ""
	}
	parts := strings.SplitN(f.sent[len(f.sent)-1], "|", 3)
	return parts[len(parts)-1]
}

// fakeClassifier is driven by the test (positive flips per reply).
type fakeClassifier struct {
	dec      Decision
	positive bool
	ack      bool
}

func (f *fakeClassifier) Classify(string, time.Time) Decision { return f.dec }
func (f *fakeClassifier) ClassifyReply(string, string) ReplyVerdict {
	return ReplyVerdict{Positive: f.positive, Ack: f.ack}
}

func newTestComp(t *testing.T, clf Classifier) (*Comp, *fakeClient, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fc := &fakeClient{}
	d := &Comp{client: fc, store: st, classify: clf, log: log.New(io.Discard, "", 0)}
	return d, fc, st
}

// reload re-reads a reminder so assertions see persisted state.
func reload(t *testing.T, st *store.Store, id int64) store.Reminder {
	t.Helper()
	r, ok, err := st.GetReminder(id)
	if err != nil || !ok {
		t.Fatalf("reload #%d: ok=%v err=%v", id, ok, err)
	}
	return r
}

// TestRoseUntilDone walks the until-done loop: pre-reminder → confirm → "not yet"
// → snooze → re-ask → "done" → done.
func TestRoseUntilDone(t *testing.T) {
	clf := &fakeClassifier{dec: Decision{Kind: "oneoff", PreDelay: time.Hour, PostGap: 15 * time.Minute}}
	d, fc, st := newTestComp(t, clf)
	now := time.Now()

	r, _ := st.CreateReminder(store.Reminder{
		RequesterAddr: "telegram|@me", TargetTransport: "telegram", TargetAddr: "100",
		TargetName: "you", TaskText: "buy a rose", Kind: "oneoff",
		PostGapSecs: 900, State: store.ReminderScheduled, NextAt: rfc(now.Add(-time.Second)),
	})

	d.advance(reload(t, st, r.ID), now)
	if got := reload(t, st, r.ID); got.State != store.ReminderPreReminded {
		t.Fatalf("after pre: %s", got.State)
	}
	if !strings.Contains(fc.lastText(), "buy a rose") || strings.Contains(fc.lastText(), "reply") {
		t.Fatalf("pre msg should mention the task naturally (no reply hint): %q", fc.lastText())
	}

	d.advance(reload(t, st, r.ID), now)
	if got := reload(t, st, r.ID); got.State != store.ReminderAwaiting {
		t.Fatalf("after confirm: %s", got.State)
	}
	if !strings.Contains(fc.lastText(), "buy a rose") || strings.Contains(fc.lastText(), "reply") {
		t.Fatalf("confirm msg should ask about the task naturally: %q", fc.lastText())
	}

	// Negative reply → snoozed.
	clf.positive = false
	if !d.FeedTelegram(100, "not yet") {
		t.Fatal("reply not consumed")
	}
	if got := reload(t, st, r.ID); got.State != store.ReminderSnoozed || got.Attempts != 1 {
		t.Fatalf("after negative: %s attempts=%d", got.State, got.Attempts)
	}

	// Snooze tick → re-ask.
	d.advance(reload(t, st, r.ID), now)
	if got := reload(t, st, r.ID); got.State != store.ReminderAwaiting {
		t.Fatalf("after re-ask: %s", got.State)
	}

	// Positive reply → done.
	clf.positive = true
	d.FeedTelegram(100, "done")
	if got := reload(t, st, r.ID); got.State != store.ReminderDone {
		t.Fatalf("after positive: %s", got.State)
	}
}

// TestAckDoesNotClose: replying "okay" to a nudge acknowledges without closing
// or snoozing — the reminder stays engaged and keeps its schedule.
func TestAckDoesNotClose(t *testing.T) {
	clf := &fakeClassifier{} // fakeClassifier returns Ack for ack-ish replies below
	d, _, st := newTestComp(t, clf)
	now := time.Now()
	r, _ := st.CreateReminder(store.Reminder{
		RequesterAddr: "telegram|@me", TargetTransport: "telegram", TargetAddr: "600",
		TargetName: "you", TaskText: "drink water", Kind: "oneoff",
		State: store.ReminderPreReminded, NextAt: rfc(now.Add(time.Hour)),
	})
	clf.ack = true
	if !d.FeedTelegram(600, "okay") {
		t.Fatal("ack not consumed")
	}
	got := reload(t, st, r.ID)
	if got.State != store.ReminderPreReminded {
		t.Fatalf("ack should keep state pre_reminded, got %s", got.State)
	}
	if got.LastReply != "okay" {
		t.Fatalf("ack should record last_reply, got %q", got.LastReply)
	}
}

// TestDeadlineMissed: a deadline-bound event whose window has passed reports
// missed (and does NOT loop) on the no-reply timeout.
func TestDeadlineMissed(t *testing.T) {
	clf := &fakeClassifier{}
	d, fc, st := newTestComp(t, clf)
	now := time.Now()

	r, _ := st.CreateReminder(store.Reminder{
		RequesterAddr: "telegram|@me", TargetTransport: "telegram", TargetAddr: "200",
		TargetName: "you", TaskText: "the 3pm sync", Kind: "oneoff", DeadlineBound: true,
		EventAt: rfc(now.Add(-time.Hour)), PostGapSecs: 1200,
		State: store.ReminderAwaiting, NextAt: rfc(now.Add(-time.Second)),
	})

	d.advance(reload(t, st, r.ID), now) // awaiting + no reply + event passed
	got := reload(t, st, r.ID)
	if got.State != store.ReminderMissed {
		t.Fatalf("expected missed, got %s", got.State)
	}
	if !strings.Contains(fc.lastText(), "hand") {
		t.Fatalf("missed msg should gently offer help: %q", fc.lastText())
	}
}

// TestRecurringReschedules: a recurring reminder fires and re-arms its next
// occurrence rather than entering a confirm loop.
func TestRecurringReschedules(t *testing.T) {
	clf := &fakeClassifier{}
	d, _, st := newTestComp(t, clf)
	now := time.Now()

	r, _ := st.CreateReminder(store.Reminder{
		RequesterAddr: "telegram|@me", TargetTransport: "telegram", TargetAddr: "300",
		TargetName: "you", TaskText: "standup", Kind: "recurring", RecurSpec: "daily 09:00",
		State: store.ReminderScheduled, NextAt: rfc(now.Add(-time.Second)),
	})

	d.advance(reload(t, st, r.ID), now)
	got := reload(t, st, r.ID)
	if got.State != store.ReminderScheduled {
		t.Fatalf("recurring should stay scheduled, got %s", got.State)
	}
	next, _ := time.Parse(time.RFC3339, got.NextAt)
	if !next.After(now) {
		t.Fatalf("next occurrence not in the future: %v", next)
	}
}

// TestThirdPartyReport: a confirmed third-party reminder reports back to the
// requester (a self reminder does not).
func TestThirdPartyReport(t *testing.T) {
	clf := &fakeClassifier{positive: true}
	d, fc, st := newTestComp(t, clf)
	now := time.Now()

	r, _ := st.CreateReminder(store.Reminder{
		RequesterAddr: "telegram|@ali", TargetContactID: 7, TargetTransport: "telegram",
		TargetAddr: "400", TargetName: "Sara", TaskText: "send the invoice", Kind: "oneoff",
		State: store.ReminderAwaiting, NextAt: rfc(now.Add(time.Hour)),
	})
	d.FeedTelegram(400, "done")

	if got := reload(t, st, r.ID); got.State != store.ReminderDone {
		t.Fatalf("state %s", got.State)
	}
	var reportedToAli bool
	for _, s := range fc.sent {
		if strings.HasPrefix(s, "telegram|@ali|") && strings.Contains(s, "is done") {
			reportedToAli = true
		}
	}
	if !reportedToAli {
		t.Fatalf("requester not sent a closing report; sends=%v", fc.sent)
	}
}

// TestOptOut: a "stop" reply cancels the reminder.
func TestOptOut(t *testing.T) {
	clf := &fakeClassifier{}
	d, _, st := newTestComp(t, clf)
	now := time.Now()
	r, _ := st.CreateReminder(store.Reminder{
		RequesterAddr: "telegram|@me", TargetTransport: "telegram", TargetAddr: "500",
		TaskText: "water plants", Kind: "oneoff",
		State: store.ReminderAwaiting, NextAt: rfc(now.Add(time.Hour)),
	})
	d.FeedTelegram(500, "stop")
	if got := reload(t, st, r.ID); got.State != store.ReminderCancelled {
		t.Fatalf("expected cancelled, got %s", got.State)
	}
}

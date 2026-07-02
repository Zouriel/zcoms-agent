package reschedule

import (
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

type fakeClient struct {
	mu       sync.Mutex
	sent     []string // "to|text"
	marks    int
	contacts []client.Contact
}

func (f *fakeClient) SendOn(transport, to, text string) (client.Response, error) {
	f.mu.Lock()
	f.sent = append(f.sent, to+"|"+text)
	f.mu.Unlock()
	return client.Response{}, nil
}
func (f *fakeClient) MarkRead(int64, []int64) error             { f.marks++; return nil }
func (f *fakeClient) MarkReadOn(string, string, []string) error { f.marks++; return nil }
func (f *fakeClient) Resolve(string) (int64, error)             { return 999, nil }
func (f *fakeClient) ResolveContact(name string) ([]client.Contact, error) {
	var out []client.Contact
	for _, c := range f.contacts {
		if strings.HasPrefix(strings.ToLower(c.Name), strings.ToLower(name)) {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeClient) sentTo(to string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		if strings.HasPrefix(s, to+"|") {
			out = append(out, strings.SplitN(s, "|", 2)[1])
		}
	}
	return out
}

type fakeTurn struct {
	outs  []string
	calls atomic.Int32
}

func (f *fakeTurn) run(prompt, resumeID string) (string, string, error) {
	n := int(f.calls.Add(1)) - 1
	o := ""
	if n < len(f.outs) {
		o = f.outs[n]
	}
	return o, "sess", nil
}

func newTestComp(t *testing.T, turn AgentTurn) (*Comp, *fakeClient, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fc := &fakeClient{}
	d := New(fc, st, 999, nil, turn)
	d.log = log.New(io.Discard, "", 0)
	d.convWaitOverride = 2 * time.Second
	return d, fc, st
}

func newEvent(t *testing.T, st *store.Store, other string) store.Reminder {
	t.Helper()
	start := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)
	r, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|@owner", RecipientTransport: "telegram", RecipientAddr: "999",
		Task: "dentist", State: store.ReminderActive, EventStart: start, NextAt: start, OtherParty: other,
	})
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	return r
}

func waitForOwn(t *testing.T, d *Comp, chatID int64, want bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if d.Owns(chatID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Owns(%d) never became %v", chatID, want)
}

// Start refuses cleanly when the event has no other party or the contact can't be
// resolved to exactly one person, without spinning a negotiation.
func TestStartGuards(t *testing.T) {
	d, fc, st := newTestComp(t, (&fakeTurn{}).run)

	if got := d.Start(4242, "note"); !strings.Contains(got, "No event") {
		t.Fatalf("missing event: %q", got)
	}
	noParty := newEvent(t, st, "")
	if got := d.Start(noParty.ID, ""); !strings.Contains(got, "no other party") {
		t.Fatalf("no other party: %q", got)
	}
	unknown := newEvent(t, st, "Ghost")
	if got := d.Start(unknown.ID, ""); !strings.Contains(got, "Couldn't find a contact") {
		t.Fatalf("unresolved contact: %q", got)
	}
	if len(fc.sent) != 0 {
		t.Fatalf("guards should send nothing, got %v", fc.sent)
	}
}

// A full negotiation: open with one message, the other party replies, agree a new
// time, update the event, and report to the owner. One message per turn.
func TestNegotiationUpdatesEvent(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"SAY: Hey! Any chance we move our dentist plan?\nDONE: no",
		"SAY: Perfect, let's do it then.\nDONE: yes\nREPORT: Sam is happy to move it.\nNEWTIME: +3h",
	}}
	d, fc, st := newTestComp(t, ft.run)
	fc.contacts = []client.Contact{{ID: 555, Name: "Sam", Telegram: "@sam"}}
	ev := newEvent(t, st, "Sam")
	oldStart := reload(t, st, ev.ID).EventStart

	if got := d.Start(ev.ID, "try for later today"); !strings.HasPrefix(got, "📆") {
		t.Fatalf("start: %q", got)
	}

	waitForOwn(t, d, 555, true)
	if !d.FeedTelegram(555, 1, "sure, when?") {
		t.Fatal("reply not consumed")
	}
	// Let the finishing turn run.
	for i := 0; i < 400 && ft.calls.Load() < 2; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	// Small settle for the report + store write.
	time.Sleep(30 * time.Millisecond)

	// Exactly two messages went to the other party (one per turn).
	if to := fc.sentTo("555"); len(to) != 2 {
		t.Fatalf("want 2 messages to the other party, got %v", to)
	}
	// The owner got a report that the event moved.
	owner := strings.Join(fc.sentTo("999"), " ")
	if !strings.Contains(owner, "moved it to") || !strings.Contains(owner, "Sam is happy") {
		t.Fatalf("owner report missing: %q", owner)
	}
	// The event's time actually changed.
	if got := reload(t, st, ev.ID); got.EventStart == oldStart || got.EventStart == "" {
		t.Fatalf("event time not updated: was %q now %q", oldStart, got.EventStart)
	}
}

func reload(t *testing.T, st *store.Store, id int64) store.Reminder {
	t.Helper()
	r, ok, err := st.GetReminder(id)
	if err != nil || !ok {
		t.Fatalf("reload #%d: ok=%v err=%v", id, ok, err)
	}
	return r
}

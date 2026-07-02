package morning

import (
	"io"
	"log"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

type fakeClient struct {
	sent  []string
	marks int
}

func (f *fakeClient) SendOn(transport, to, text string) (client.Response, error) {
	f.sent = append(f.sent, text)
	return client.Response{}, nil
}
func (f *fakeClient) MarkRead(int64, []int64) error             { f.marks++; return nil }
func (f *fakeClient) MarkReadOn(string, string, []string) error { f.marks++; return nil }
func (f *fakeClient) Resolve(string) (int64, error)             { return 999, nil }

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
	d := New(fc, st, "@owner", 999, nil, turn)
	d.log = log.New(io.Discard, "", 0)
	d.wakeWaitOverride = 2 * time.Second
	d.convWaitOverride = 2 * time.Second
	return d, fc, st
}

func waitForOwn(t *testing.T, d *Comp, want bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if d.Owns(999) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Owns never became %v", want)
}

func waitForCalls(t *testing.T, ft *fakeTurn, n int32) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ft.calls.Load() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("turn count never reached %d (got %d)", n, ft.calls.Load())
}

// A full briefing: greet, wait for the owner to wake, present the day, then act on
// an "add" request and close the session.
func TestBriefingAddsEvent(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"SAY: Good morning!",
		"SAY: Morning! Your day is open so far. Want to add anything?\nACTION: none\nTASK: -\nSTART: -\nEND: -\nOTHER: -\nID: -\nEND_SESSION: no",
		"SAY: Done, dentist at 3.\nACTION: add\nTASK: dentist\nSTART: 15:00\nEND: -\nOTHER: Dr Lee\nID: -\nEND_SESSION: yes",
	}}
	d, fc, st := newTestComp(t, ft.run)

	done := make(chan struct{})
	go func() { d.runSession(); close(done) }()

	// It greets (turn 1), then parks waiting for the owner to wake.
	waitForOwn(t, d, true)
	if !d.FeedTelegram(999, 1, "morning") {
		t.Fatal("wake reply not consumed")
	}
	// It briefs (turn 2, ACTION none); wait for that turn so the wake channel is
	// gone before we send the next message, then park again on the conversation.
	waitForCalls(t, ft, 2)
	waitForOwn(t, d, true)
	if !d.FeedTelegram(999, 2, "add a dentist appointment at 3pm") {
		t.Fatal("follow-up not consumed")
	}
	<-done

	if n := ft.calls.Load(); n != 3 {
		t.Fatalf("want 3 turns (greet, brief, act), got %d", n)
	}
	rs, err := st.ListReminders()
	if err != nil || len(rs) != 1 {
		t.Fatalf("want 1 event created, got %d (err %v)", len(rs), err)
	}
	r := rs[0]
	if r.Task != "dentist" || r.OtherParty != "Dr Lee" || r.EventStart == "" {
		t.Fatalf("event not stored as expected: %+v", r)
	}
	if _, err := time.Parse(time.RFC3339, r.EventStart); err != nil {
		t.Fatalf("event_start not RFC3339: %q", r.EventStart)
	}
	if fc.marks == 0 {
		t.Fatal("consumed replies should be marked read")
	}
}

// With no backend (nil turn) Fire is a no-op and never sends.
func TestFireNoBackendIsNoop(t *testing.T) {
	d, fc, _ := newTestComp(t, nil)
	d.turn = nil
	d.Fire()
	time.Sleep(20 * time.Millisecond)
	if len(fc.sent) != 0 {
		t.Fatalf("no-backend Fire should send nothing, got %v", fc.sent)
	}
}

// The enable/disable toggle is read live from the store.
func TestConfigToggle(t *testing.T) {
	_, _, st := newTestComp(t, (&fakeTurn{}).run)
	if !LoadConfig(st).Enabled {
		t.Fatal("default should be enabled")
	}
	if err := st.SetSetting(store.Owner, KeyEnabled, "false"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if LoadConfig(st).Enabled {
		t.Fatal("should be disabled after setting false")
	}
}

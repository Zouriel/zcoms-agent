package lineup

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
	mu    sync.Mutex
	sent  []string
	marks int
}

func (f *fakeClient) SendOn(transport, to, text string) (client.Response, error) {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	return client.Response{}, nil
}
func (f *fakeClient) MarkRead(int64, []int64) error { f.marks++; return nil }
func (f *fakeClient) Resolve(string) (int64, error) { return 999, nil }
func (f *fakeClient) sentCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
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
	d := New(fc, st, "@owner", 999, nil, turn)
	d.log = log.New(io.Discard, "", 0)
	return d, fc, st
}

func (d *Comp) isActive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}

func (d *Comp) isBusy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.busy
}

func waitActive(t *testing.T, d *Comp, want bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if d.isActive() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("active never became %v", want)
}

func waitCalls(t *testing.T, ft *fakeTurn, n int32) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ft.calls.Load() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("turn count never reached %d (got %d)", n, ft.calls.Load())
}

func waitNotBusy(t *testing.T, d *Comp) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if !d.isBusy() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("turn never finished (still busy)")
}

func TestSplitKeyword(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		rest string
	}{
		{"lineup", true, ""},
		{"lineup what's on today", true, "what's on today"},
		{"LINEUP: add gym at 6", true, "add gym at 6"},
		{"lineup, move my meeting", true, "move my meeting"},
		{"  lineup   spaced  ", true, "spaced"},
		{"lineups", false, ""},
		{"line up", false, ""},
		{"remind me", false, ""},
	}
	for _, c := range cases {
		ok, rest := splitKeyword(c.in)
		if ok != c.ok || rest != c.rest {
			t.Errorf("splitKeyword(%q) = (%v,%q), want (%v,%q)", c.in, ok, rest, c.ok, c.rest)
		}
	}
}

// The turn prompt hands the model the events near the asked-about time and tells
// it to flag clashes, so "can I do a date at 6?" with a 7pm event on the calendar
// gets a gap-aware answer rather than a blind yes.
func TestPromptIsScheduleAware(t *testing.T) {
	now := time.Now()
	sevenPM := time.Date(now.Year(), now.Month(), now.Day(), 19, 0, 0, 0, now.Location())
	ev := store.Reminder{ID: 42, Task: "standup", EventStart: sevenPM.UTC().Format(time.RFC3339)}

	p := lineupPrompt([]store.Reminder{ev}, nil, "can I go on a date with this girl at 6?", true, now)

	for _, want := range []string{
		"7:00 PM",                  // the 7pm event is in front of the model
		"standup",                  // named, so it can reference it
		"date with this girl at 6", // the owner's actual question
		"schedule-aware",           // the instruction to check nearby times
		"SAFE MARGIN",              // enforce a buffer between events
		"30 minutes",               // the default margin size
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

// A non-owner chat, and an ordinary owner message with no open session, are left
// for the bridge (Trigger returns false and runs no turn).
func TestTriggerLeavesOrdinaryTrafficToBridge(t *testing.T) {
	ft := &fakeTurn{}
	d, _, _ := newTestComp(t, ft.run)

	if d.Trigger(555, 1, "lineup hi") { // not the owner's chat
		t.Fatal("non-owner should not be captured")
	}
	if d.Trigger(999, 1, "what's the weather") { // owner, no keyword, no session
		t.Fatal("ordinary owner message should fall through to the bridge")
	}
	if ft.calls.Load() != 0 {
		t.Fatalf("no turns should have run, got %d", ft.calls.Load())
	}
}

// The keyword opens a session; every later message continues it (adding an event
// along the way), and it stays open until the owner says "end".
func TestOpensRunsAndEnds(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"SAY: Today's clear so far.\nACTION: none\nTASK: -\nSTART: -\nEND: -\nOTHER: -\nID: -",
		"SAY: Added, gym at 6pm.\nACTION: add\nTASK: gym\nSTART: 18:00\nEND: -\nOTHER: -\nID: -",
	}}
	d, fc, st := newTestComp(t, ft.run)

	// Keyword opens with a request → turn 1 (read the agenda).
	if !d.Trigger(999, 1, "lineup what's on today") {
		t.Fatal("keyword should open the lineup")
	}
	waitCalls(t, ft, 1)
	waitNotBusy(t, d)
	if !d.isActive() {
		t.Fatal("session should be open after the keyword")
	}

	// A follow-up with no keyword continues it → turn 2 (add an event).
	if !d.Trigger(999, 2, "add gym at 6pm") {
		t.Fatal("follow-up should be consumed by the open session")
	}
	waitCalls(t, ft, 2)
	waitNotBusy(t, d)
	if !d.isActive() {
		t.Fatal("session should stay open until the owner ends it")
	}

	rs, err := st.ListReminders()
	if err != nil || len(rs) != 1 {
		t.Fatalf("want 1 event added, got %d (err %v)", len(rs), err)
	}
	if rs[0].Task != "gym" || rs[0].EventStart == "" {
		t.Fatalf("event not stored as expected: %+v", rs[0])
	}

	// "end" closes it and runs no model turn.
	if !d.Trigger(999, 3, "end") {
		t.Fatal("end should be consumed by the session")
	}
	waitActive(t, d, false)
	if got := ft.calls.Load(); got != 2 {
		t.Fatalf("end must not run a turn; want 2 turns, got %d", got)
	}
	if fc.marks == 0 {
		t.Fatal("consumed messages should be marked read")
	}
}

// After a quiet stretch the session closes itself and says so, and later messages
// with no keyword fall back to the bridge.
func TestIdleTimeoutCloses(t *testing.T) {
	ft := &fakeTurn{outs: []string{"SAY: Here's your day.\nACTION: none"}}
	d, fc, _ := newTestComp(t, ft.run)
	d.idleWait = 40 * time.Millisecond

	if !d.Trigger(999, 1, "lineup") {
		t.Fatal("keyword should open the lineup")
	}
	waitCalls(t, ft, 1)
	waitNotBusy(t, d)

	// No further messages: the idle timer should close it.
	waitActive(t, d, false)
	joined := strings.ToLower(strings.Join(fc.sentCopy(), "\n"))
	if !strings.Contains(joined, "close the lineup") {
		t.Fatalf("expected an idle-close notice, got %q", joined)
	}

	// A plain message now falls through (session is closed).
	if d.Trigger(999, 2, "still there?") {
		t.Fatal("after idle close, an ordinary message should go to the bridge")
	}
}

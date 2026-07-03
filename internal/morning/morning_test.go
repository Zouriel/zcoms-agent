package morning

import (
	"io"
	"log"
	"path/filepath"
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
func (f *fakeClient) MarkRead(int64, []int64) error             { f.marks++; return nil }
func (f *fakeClient) MarkReadOn(string, string, []string) error { f.marks++; return nil }
func (f *fakeClient) Resolve(string) (int64, error)             { return 999, nil }
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

func (d *Comp) isBusy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.busy
}

func waitNotBusy(t *testing.T, d *Comp) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if !d.isBusy() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("turn never finished (still busy)")
}

// The morning session greets, then stays open across replies with no timeout:
// the first reply briefs, a later reply adds an event, and it only closes when
// the owner says "end" (the default bridge behaviour). One turn per message.
func TestSessionStaysOpenUntilEnd(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"SAY: Good morning!",
		"SAY: Morning! Your day is open so far. Want to add anything?\nACTION: none\nTASK: -\nSTART: -\nEND: -\nOTHER: -\nID: -",
		"SAY: Done, dentist at 3.\nACTION: add\nTASK: dentist\nSTART: 15:00\nLEAD: 45m\nEND: -\nOTHER: Dr Lee\nID: -",
	}}
	d, fc, st := newTestComp(t, ft.run)

	// Fire greets (turn 1) and then goes dormant: the session is open but no
	// goroutine is parked.
	d.Fire()
	waitForOwn(t, d, true)
	waitForCalls(t, ft, 1)
	waitNotBusy(t, d)

	// First reply → briefing (turn 2). Session stays open.
	if !d.FeedTelegram(999, 1, "morning") {
		t.Fatal("wake reply not consumed")
	}
	waitForCalls(t, ft, 2)
	waitNotBusy(t, d)
	if !d.Owns(999) {
		t.Fatal("session should stay open after the briefing (no timeout)")
	}

	// A much later reply → add an event (turn 3). Still open.
	if !d.FeedTelegram(999, 2, "add a dentist appointment at 3pm") {
		t.Fatal("follow-up not consumed")
	}
	waitForCalls(t, ft, 3)
	waitNotBusy(t, d)
	if !d.Owns(999) {
		t.Fatal("session should stay open until the owner ends it")
	}

	rs, err := st.ListReminders()
	if err != nil || len(rs) != 1 {
		t.Fatalf("want 1 event created, got %d (err %v)", len(rs), err)
	}
	r := rs[0]
	if r.Task != "dentist" || r.OtherParty != "Dr Lee" || r.EventStart == "" {
		t.Fatalf("event not stored as expected: %+v", r)
	}
	// NextAt (the nudge) must land the LEAD (45m) before EventStart, not at it.
	es, err1 := time.Parse(time.RFC3339, r.EventStart)
	na, err2 := time.Parse(time.RFC3339, r.NextAt)
	if err1 != nil || err2 != nil {
		t.Fatalf("event_start/next_at not RFC3339: start=%q next=%q", r.EventStart, r.NextAt)
	}
	if d := es.Sub(na); d != 45*time.Minute {
		t.Fatalf("want NextAt 45m before EventStart, got %v (start=%v next=%v)", d, es, na)
	}

	// Only "end" closes it; the model never got to.
	if !d.FeedTelegram(999, 3, "end") {
		t.Fatal("end not consumed")
	}
	waitForOwn(t, d, false)
	if got := ft.calls.Load(); got != 3 {
		t.Fatalf("end must not run a model turn; want 3 turns, got %d", got)
	}
	if fc.marks == 0 {
		t.Fatal("consumed replies should be marked read")
	}
}

// A message that lands mid-turn is nudged, not dropped into a second concurrent
// turn, and does not fall through to the bridge.
func TestBusyNudgesInsteadOfDropping(t *testing.T) {
	// Fire leaves busy=true across the greeting; the reply below arrives before it
	// clears, so it must be consumed with a nudge and run no turn.
	ft := &fakeTurn{outs: []string{"SAY: Good morning!"}}
	d, fc, _ := newTestComp(t, ft.run)

	d.mu.Lock()
	d.active, d.first, d.busy = true, true, true
	d.mu.Unlock()

	if !d.FeedTelegram(999, 1, "hey") {
		t.Fatal("busy reply should still be consumed by the session")
	}
	if ft.calls.Load() != 0 {
		t.Fatalf("no model turn should run while busy, got %d", ft.calls.Load())
	}
	sent := fc.sentCopy()
	if len(sent) != 1 || sent[0] != "One sec, still finishing that." {
		t.Fatalf("want a single nudge, got %v", sent)
	}
}

// With no backend (nil turn) Fire is a no-op and never sends.
func TestFireNoBackendIsNoop(t *testing.T) {
	d, fc, _ := newTestComp(t, nil)
	d.turn = nil
	d.Fire()
	time.Sleep(20 * time.Millisecond)
	if len(fc.sentCopy()) != 0 {
		t.Fatalf("no-backend Fire should send nothing, got %v", fc.sentCopy())
	}
	if d.Owns(999) {
		t.Fatal("no-backend Fire should not open a session")
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

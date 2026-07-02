package reminders

import (
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

func newReminder(t *testing.T, st *store.Store, recipientAddr string, contactID int64) store.Reminder {
	t.Helper()
	r, err := st.CreateReminder(store.Reminder{
		FromAddr: "telegram|@owner", FromName: "you",
		RecipientTransport: "telegram", RecipientAddr: recipientAddr, RecipientContactID: contactID,
		Task: "get ready for the gym at 8", State: store.ReminderActive,
		NextAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return r
}

// A planning run (SEND: NONE) sends nothing and just reschedules from turn 1.
func TestPlanningRun(t *testing.T) {
	ft := &fakeTurn{outs: []string{"SEND: NONE\nNEXT: +30m\nNOTE: too early, nudge closer to 8"}}
	d, fc, st := newTestComp(t, ft.run)
	r := newReminder(t, st, "100", 0)

	d.runReminder(r)

	if len(fc.sent) != 0 {
		t.Fatalf("planning run should send nothing, got %v", fc.sent)
	}
	if ft.calls != 1 {
		t.Fatalf("planning run should be one turn, got %d", ft.calls)
	}
	got := reload(t, st, r.ID)
	if got.State != store.ReminderActive || got.CarryOver == "" {
		t.Fatalf("row: %+v", got)
	}
	at, err := time.Parse(time.RFC3339, got.NextAt)
	if err != nil || !at.After(time.Now()) {
		t.Fatalf("next_at not future: %q", got.NextAt)
	}
	// A quiet planning run sends no nudge, so it must not spend against the cap.
	if got.Runs != 0 {
		t.Fatalf("quiet run spent a nudge: runs = %d", got.Runs)
	}
}

// The nudge cap counts only delivered nudges and resets when the recipient
// replies, so a reminder that keeps getting engagement is never killed by it,
// while one that pesters an unresponsive recipient eventually stops.
func TestNudgeCapCountsAndResets(t *testing.T) {
	// A nudge that gets a reply: Runs is bumped for the send, then reset to 0.
	ft := &fakeTurn{outs: []string{
		"SEND: gym at 8, get moving\nNEXT: +1h\nNOTE: nudged",
		"REPLY: NONE\nNEXT: +1d\nNOTE: recurring, check tomorrow",
	}}
	d, _, st := newTestComp(t, ft.run)
	d.replyWaitOverride = 2 * time.Second
	r := newReminder(t, st, "500", 0)
	r.Runs = 5 // pretend it had nudged a few times already
	d.save(r)

	done := make(chan struct{})
	go func() { d.runReminder(reload(t, st, r.ID)); close(done) }()
	waitForOwn(t, d, 500, true)
	if !d.FeedTelegram(500, 1, "on it") {
		t.Fatal("reply not consumed")
	}
	<-done

	if got := reload(t, st, r.ID); got.Runs != 0 {
		t.Fatalf("a reply should reset the nudge count, got runs = %d", got.Runs)
	}

	// At the cap with no reply in sight, the reminder gives up rather than pester on.
	ft2 := &fakeTurn{outs: []string{"SEND: still waiting\nNEXT: +1h\nNOTE: x"}}
	d2, fc2, st2 := newTestComp(t, ft2.run)
	capped := newReminder(t, st2, "600", 0)
	capped.Runs = d2.cfg().MaxRuns
	d2.save(capped)
	d2.runReminder(reload(t, st2, capped.ID))
	if got := reload(t, st2, capped.ID); got.State != store.ReminderDone {
		t.Fatalf("at the cap it should finish, state = %s", got.State)
	}
	if len(fc2.sent) != 0 {
		t.Fatalf("a capped reminder should not send another nudge, got %v", fc2.sent)
	}
}

// A nudge run: turn 1 sends, the recipient replies, turn 2 closes it done.
func TestNudgeReplyDone(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"SEND: Hey, gym time is coming up at 8. Start getting ready soon.\nNEXT: +1h\nNOTE: nudged, waiting",
		"REPLY: Nice, have a good one!\nNEXT: DONE\nNOTE: they're heading out, done",
	}}
	d, fc, st := newTestComp(t, ft.run)
	d.replyWaitOverride = 2 * time.Second
	r := newReminder(t, st, "200", 0)

	done := make(chan struct{})
	go func() { d.runReminder(r); close(done) }()

	// Wait until the run is parked on the reply, then feed it.
	waitForOwn(t, d, 200, true)
	if !d.FeedTelegram(200, 4242, "I'm in the gym now") {
		t.Fatal("reply not consumed")
	}
	<-done

	if fc.marks == 0 {
		t.Fatal("the consumed Telegram reply should be marked read")
	}

	got := reload(t, st, r.ID)
	if got.State != store.ReminderDone {
		t.Fatalf("state = %s, want done", got.State)
	}
	if ft.calls != 2 {
		t.Fatalf("want 2 turns, got %d", ft.calls)
	}
	// Both the nudge and the closing reply were sent.
	if len(fc.sent) != 2 {
		t.Fatalf("want 2 sends, got %v", fc.sent)
	}
	// Not waiting anymore.
	if d.Owns(200) {
		t.Fatal("still owns the chat after the run")
	}
}

// No reply within the window: turn 2 still runs (with the silence) and decides.
func TestNoReplyTimeout(t *testing.T) {
	ft := &fakeTurn{outs: []string{
		"SEND: Did you make it?\nNEXT: +1h\nNOTE: asked",
		"REPLY: NONE\nNEXT: +2h\nNOTE: no reply, check again later",
	}}
	d, _, st := newTestComp(t, ft.run)
	d.replyWaitOverride = 40 * time.Millisecond
	r := newReminder(t, st, "300", 0)

	d.runReminder(r) // blocks ~40ms on the (empty) reply, then reacts

	if ft.calls != 2 {
		t.Fatalf("want 2 turns even with no reply, got %d", ft.calls)
	}
	got := reload(t, st, r.ID)
	if got.State != store.ReminderActive || got.CarryOver == "" {
		t.Fatalf("row: %+v", got)
	}
}

func waitForOwn(t *testing.T, d *Comp, chatID int64, want bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if d.Owns(chatID) == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("Owns(%d) never became %v", chatID, want)
}

func TestParseNext(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.Local)
	if s, _ := parseNext("DONE", now); s != store.ReminderDone {
		t.Errorf("DONE -> %s", s)
	}
	if s, _ := parseNext("cancel", now); s != store.ReminderCancelled {
		t.Errorf("cancel -> %s", s)
	}
	s, at := parseNext("+30m", now)
	if s != store.ReminderActive || !at.Equal(now.Add(30*time.Minute)) {
		t.Errorf("+30m -> %s %v", s, at)
	}
	if _, at := parseNext("+1d", now); !at.Equal(now.AddDate(0, 0, 1)) {
		t.Errorf("+1d -> %v", at)
	}
	// Unparseable stays active with a fallback delay (never stuck).
	if s, at := parseNext("whenever", now); s != store.ReminderActive || !at.After(now) {
		t.Errorf("garbage -> %s %v", s, at)
	}
}

func TestParseFields(t *testing.T) {
	f := parseFields("SEND: Hey there, all good?\nNEXT: +2h\nNOTE: nudged once")
	if f["send"] != "Hey there, all good?" || f["next"] != "+2h" || f["note"] != "nudged once" {
		t.Fatalf("parsed: %#v", f)
	}
}

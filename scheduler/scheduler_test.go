package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestInterval verifies fn fires after one interval (not immediately) and keeps
// firing. We use a tiny duration and drive evaluate() directly with synthetic
// times so the test is fast and deterministic.
func TestIntervalFiresAfterInterval(t *testing.T) {
	s := New()
	var count int32
	s.Interval("poll", 20*time.Millisecond, func() { atomic.AddInt32(&count, 1) })

	base := time.Now()
	// First tick only arms the interval — must not fire immediately.
	s.evaluate(base)
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Fatalf("interval fired immediately on first tick, count=%d", got)
	}
	// A tick before the deadline must not fire.
	s.evaluate(base.Add(10 * time.Millisecond))
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Fatalf("interval fired before deadline, count=%d", got)
	}
	// A tick past the deadline fires once.
	s.evaluate(base.Add(25 * time.Millisecond))
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("interval did not fire after deadline, count=%d", got)
	}
	// Next interval fires again.
	s.evaluate(base.Add(50 * time.Millisecond))
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("interval did not re-fire, count=%d", got)
	}
}

// TestIntervalViaRun is a small end-to-end sanity check that Run actually ticks
// and invokes a job. Uses a short interval and a channel so it stays fast.
func TestIntervalViaRun(t *testing.T) {
	// Shrink the tick so Run evaluates quickly. tickInterval is a package const,
	// so we exercise Run through a job whose interval is shorter than one tick;
	// the first real tick (30s) is too slow for a unit test, so instead we assert
	// the loop returns promptly on cancel and rely on the synthetic tests above
	// for firing semantics.
	s := New()
	fired := make(chan struct{}, 1)
	s.Interval("p", time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestDueDailyGuard(t *testing.T) {
	tz := time.UTC
	day := time.Date(2026, 6, 29, 0, 0, 0, 0, tz)

	// Before the target time: not due.
	if dueDaily("", day.Add(8*time.Hour), 9, 0) {
		t.Fatal("daily fired before target hh:mm")
	}
	// At/after the target time, never fired today: due.
	if !dueDaily("", day.Add(9*time.Hour), 9, 0) {
		t.Fatal("daily did not fire at target hh:mm")
	}
	// Already fired today (guard set to today's date): not due again, even later.
	if dueDaily("2026-06-29", day.Add(23*time.Hour), 9, 0) {
		t.Fatal("daily fired twice on the same day")
	}
	// Guard is from a previous day: due again next day after the target.
	next := day.AddDate(0, 0, 1).Add(9 * time.Hour)
	if !dueDaily("2026-06-29", next, 9, 0) {
		t.Fatal("daily did not fire on the next day")
	}
}

// TestDailyAtMarkPreventsSecondFire exercises DailyAt through the scheduler:
// once it fires and markFired records today's date, a later tick the same day
// must not fire again.
func TestDailyAtMarkPreventsSecondFire(t *testing.T) {
	s := New()
	var count int32
	s.DailyAt("standup", 9, 0, time.UTC, func() { atomic.AddInt32(&count, 1) })

	day := time.Date(2026, 6, 29, 9, 30, 0, 0, time.UTC)
	s.evaluate(day)
	s.evaluate(day.Add(time.Hour)) // same day, must be guarded
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("DailyAt fired %d times on one day, want 1", got)
	}
	// Next day after target: fires again.
	s.evaluate(day.AddDate(0, 0, 1))
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("DailyAt did not fire next day, count=%d", got)
	}
}

func TestDueOnce(t *testing.T) {
	at := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	if dueOnce("", at.Add(-time.Minute), at) {
		t.Fatal("once fired before its time")
	}
	if !dueOnce("", at, at) {
		t.Fatal("once did not fire at its time")
	}
	if !dueOnce("", at.Add(time.Hour), at) {
		t.Fatal("once did not fire when already past")
	}
	if dueOnce("done", at.Add(time.Hour), at) {
		t.Fatal("once fired again after being marked done")
	}
}

// TestOnceFiresOnce drives Once through the scheduler and asserts it runs a
// single time even across many ticks past the target.
func TestOnceFiresOnce(t *testing.T) {
	s := New()
	var count int32
	at := time.Now().Add(-time.Hour) // already past
	s.Once("kickoff", at, func() { atomic.AddInt32(&count, 1) })

	for i := 0; i < 5; i++ {
		s.evaluate(time.Now())
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("Once fired %d times, want 1", got)
	}
}

// TestPanicIsolation verifies a panicking job is recovered and does not stop the
// other due jobs in the same tick.
func TestPanicIsolation(t *testing.T) {
	s := New()
	var ok int32
	at := time.Now().Add(-time.Hour)
	s.Once("boom", at, func() { panic("boom") })
	s.Once("safe", at, func() { atomic.AddInt32(&ok, 1) })

	// Must not panic out of evaluate, and the safe job must still run.
	s.evaluate(time.Now())
	if got := atomic.LoadInt32(&ok); got != 1 {
		t.Fatalf("panicking job blocked others, ok=%d", got)
	}
}

// TestMemoryStore is a minimal contract check on the default Store.
func TestMemoryStore(t *testing.T) {
	st := NewMemoryStore()
	if v, _ := st.Get("missing"); v != "" {
		t.Fatalf("absent key returned %q, want empty", v)
	}
	if err := st.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Get("k"); v != "v" {
		t.Fatalf("Get after Set = %q, want v", v)
	}
}

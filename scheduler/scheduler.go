// Package scheduler is a single-loop primitive that replaces the three
// independent `for { sleep; check-due }` goroutines we used to run (triage
// interval, errands one-shot, standups daily). Folding them into one ticking
// loop means one place to reason about timing, one recover() boundary per job,
// and a shared Store so the daily/once guards survive process restarts.
package scheduler

import (
	"context"
	"sync"
	"time"
)

// tickInterval is how often Run wakes to re-evaluate every job. It is coarse on
// purpose: none of our jobs need sub-minute precision, and a slow tick keeps the
// loop cheap. DailyAt/Once tolerances are written to absorb this granularity.
const tickInterval = 30 * time.Second

// Store persists the "already fired" guards for DailyAt/Once so that a restart
// (or a tick firing twice for the same target) cannot double-run a job. An empty
// string from Get means "no record" — i.e. never fired.
type Store interface {
	Get(key string) (string, error) // returns "" if absent
	Set(key, value string) error
}

// MemoryStore is the default Store for callers that don't need persistence
// across restarts (and for tests). Guards live only for the process lifetime.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{m: make(map[string]string)}
}

func (s *MemoryStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key], nil
}

func (s *MemoryStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

// job is the internal closure form every public registration compiles down to.
// due(now) decides whether fn should run this tick; markFired records that it
// did (no-op for intervals, which are stateless). Keeping the variation behind
// two function fields lets Run treat all job kinds uniformly.
type job struct {
	name      string
	due       func(now time.Time) bool
	markFired func(now time.Time)
	fn        func()
}

type Scheduler struct {
	mu    sync.Mutex
	jobs  []*job
	store Store
}

// New returns a Scheduler backed by an in-memory Store. Callers that need the
// daily/once guards to survive restarts should construct with a persistent Store
// via NewWithStore.
func New() *Scheduler {
	return NewWithStore(NewMemoryStore())
}

func NewWithStore(store Store) *Scheduler {
	return &Scheduler{store: store}
}

// Interval runs fn every d, first firing one interval after Run starts (not
// immediately). It is stateless: a restart resets the next-fire clock, which is
// the desired behavior for polling-style work like triage.
func (s *Scheduler) Interval(name string, d time.Duration, fn func()) {
	// next is captured per-job; the first deadline is set on the first tick that
	// observes it (see lazy init below) so timing is relative to Run, not to
	// registration.
	var next time.Time
	s.add(&job{
		name: name,
		due: func(now time.Time) bool {
			if next.IsZero() {
				next = now.Add(d)
				return false
			}
			if now.Before(next) {
				return false
			}
			next = now.Add(d)
			return true
		},
		markFired: func(time.Time) {},
		fn:        fn,
	})
}

// DailyAt runs fn once per calendar day at hh:mm in tz. The per-day guard is
// persisted by date string (key = name) so a restart mid-day, or multiple ticks
// landing in the firing window, still fire at most once per local day.
func (s *Scheduler) DailyAt(name string, hour, min int, tz *time.Location, fn func()) {
	if tz == nil {
		tz = time.Local
	}
	s.add(&job{
		name: name,
		due: func(now time.Time) bool {
			last, _ := s.store.Get(name)
			return dueDaily(last, now.In(tz), hour, min)
		},
		markFired: func(now time.Time) {
			_ = s.store.Set(name, dateKey(now.In(tz)))
		},
		fn: fn,
	})
}

// Once runs fn a single time once at is reached (or immediately if at is already
// in the past and it has not fired yet). The fired flag is persisted so it never
// runs twice, even across restarts.
func (s *Scheduler) Once(name string, at time.Time, fn func()) {
	s.add(&job{
		name: name,
		due: func(now time.Time) bool {
			fired, _ := s.store.Get(name)
			return dueOnce(fired, now, at)
		},
		markFired: func(time.Time) {
			_ = s.store.Set(name, "done")
		},
		fn: fn,
	})
}

func (s *Scheduler) add(j *job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, j)
}

// Run is the single ticking loop. It evaluates every registered job each tick
// and runs the due ones. It returns when ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.evaluate(now)
		}
	}
}

// evaluate runs one pass over all jobs. It snapshots the slice under the lock so
// late registrations are safe, then runs callbacks unlocked so a slow fn can't
// block registration or other jobs' due checks.
func (s *Scheduler) evaluate(now time.Time) {
	s.mu.Lock()
	jobs := make([]*job, len(s.jobs))
	copy(jobs, s.jobs)
	s.mu.Unlock()

	for _, j := range jobs {
		if j.due(now) {
			// Mark fired before invoking so a panic mid-fn can't cause an
			// infinite retry storm on every subsequent tick.
			j.markFired(now)
			runGuarded(j.fn)
		}
	}
}

// runGuarded wraps a job's fn so a panic in one job never kills the loop or
// stops the remaining jobs in this tick from running.
func runGuarded(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// dueDaily is the pure decision behind DailyAt, separated out so it can be unit
// tested with explicit `now` values instead of waiting for wall-clock time.
// now must already be in the target timezone. It is due when we've reached the
// hh:mm boundary today and today's date hasn't been recorded as fired yet.
func dueDaily(lastFired string, now time.Time, hour, min int) bool {
	if lastFired == dateKey(now) {
		return false // already fired today
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	return !now.Before(target)
}

// dueOnce is the pure decision behind Once: fire when not yet fired and the
// target instant has been reached.
func dueOnce(fired string, now, at time.Time) bool {
	if fired != "" {
		return false
	}
	return !now.Before(at)
}

// dateKey is the calendar-day guard key (local date of now). Two instants on the
// same local day share a key, which is what makes the daily guard idempotent.
func dateKey(now time.Time) string {
	return now.Format("2006-01-02")
}

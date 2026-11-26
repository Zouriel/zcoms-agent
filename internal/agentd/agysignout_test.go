package agentd

import (
	"sync"
	"testing"
	"time"
)

func TestAgyPingerRateLimits(t *testing.T) {
	var mu sync.Mutex
	sent := 0
	done := make(chan struct{}, 4)
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	p := &agyPinger{now: func() time.Time { return clock }, send: func(string) error {
		mu.Lock()
		sent++
		mu.Unlock()
		done <- struct{}{}
		return nil
	}}
	p.fire()
	p.fire()
	<-done
	clock = clock.Add(agyPingEvery + time.Minute)
	p.fire()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if sent != 2 {
		t.Fatalf("sent %d pings, want 2", sent)
	}
}

package agentd

import (
	"sync"
	"time"
)

// agyPingEvery is how often, at most, the owner is told agy is signed out.
const agyPingEvery = 6 * time.Hour

// agyPinger tells the owner, at most once per agyPingEvery, that agy runs are
// failing because the CLI is signed out (its sign-in lapses without warning).
type agyPinger struct {
	mu   sync.Mutex
	last time.Time
	now  func() time.Time
	send func(text string) error
}

const agySignedOutText = "agy (Antigravity) is signed out, so voice notes and agy runs are failing. Run `agy` on the workhost once to sign in again."

// fire sends the ping unless one went out recently. Safe from any goroutine.
func (p *agyPinger) fire() {
	p.mu.Lock()
	now := p.now()
	if !p.last.IsZero() && now.Sub(p.last) < agyPingEvery {
		p.mu.Unlock()
		return
	}
	p.last = now
	p.mu.Unlock()
	go func() { _ = p.send(agySignedOutText) }()
}

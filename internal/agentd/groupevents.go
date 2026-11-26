package agentd

import "github.com/Zouriel/zcoms/client"

// handleGroupEvent receives a group call from the subscription. Until the
// groups handler is wired, every group event is dropped here so it can never
// fall through to a 1:1 subscriber.
func (a *Agent) handleGroupEvent(ev client.Event) {}

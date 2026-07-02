package agentd

import (
	"fmt"
	"strings"
	"time"
)

// eventsCmd answers "what's on around this time" for the CLI/console, delegating
// to the reminders runtime (which owns the events store). Reached via
// `zc agent events <date-time>`.
func (a *Agent) eventsCmd(args []string) (string, error) {
	when := strings.TrimSpace(strings.Join(args, " "))
	if when == "" {
		return "", fmt.Errorf("usage: events <date and time>  (e.g. events 2026-07-04 15:00)")
	}
	return a.Reminders.EventsAround(when, time.Now())
}

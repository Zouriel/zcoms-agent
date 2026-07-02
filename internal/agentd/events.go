package agentd

import (
	"fmt"
	"strconv"
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

// rescheduleCmd starts a negotiation to move an event with its other party.
// Reached via `zc agent reschedule <event id> | <note>` (the note, the brief for
// the negotiator, is everything after the id, with an optional leading "|").
func (a *Agent) rescheduleCmd(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("usage: reschedule <event id> | <note>")
	}
	id, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil {
		return "", fmt.Errorf("first argument must be the event id: %w", err)
	}
	note := strings.TrimSpace(strings.Join(args[1:], " "))
	note = strings.TrimSpace(strings.TrimPrefix(note, "|"))
	return a.Reschedule.Start(id, note), nil
}

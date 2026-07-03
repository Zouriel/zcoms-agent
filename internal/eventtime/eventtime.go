// Package eventtime separates an event's own time from its reminder fire time.
// EventStart is when the thing actually happens; NextAt is when to nudge the
// owner, set a lead window earlier so they have time to get ready. The lead is
// chosen per event by whichever agent registers it (a quick call needs minutes,
// something across town needs an hour), floored so there is always a heads-up.
package eventtime

import (
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultLead is the heads-up used when the registering agent gives none.
	DefaultLead = 30 * time.Minute
	// MinLead is the floor: a reminder never lands later than this before the event.
	MinLead = 15 * time.Minute
)

// ParseLead reads a heads-up window ("30m", "1h", "45 min", "1 hour", "20 minutes"),
// returning DefaultLead when blank or unparseable, and never less than MinLead.
func ParseLead(s string) time.Duration {
	d, ok := parseDur(s)
	if !ok {
		return DefaultLead
	}
	if d < MinLead {
		return MinLead
	}
	return d
}

// NextAt returns the RFC3339 reminder time for an event: eventStart minus the
// chosen lead. If eventStart is blank or not RFC3339 (an event with no set time),
// it is returned unchanged, since there is no anchor to lead from.
func NextAt(eventStartRFC, lead string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(eventStartRFC))
	if err != nil {
		return eventStartRFC
	}
	return t.Add(-ParseLead(lead)).Format(time.RFC3339)
}

// Shift returns the reminder time when an event moves to newStart, preserving the
// event's existing lead (oldStart - oldNextAt), floored at MinLead and defaulted
// when the old row had none (e.g. legacy rows where NextAt == EventStart). A blank
// or unparseable newStart is returned unchanged.
func Shift(oldStartRFC, oldNextAtRFC, newStartRFC string) string {
	ns, err := time.Parse(time.RFC3339, strings.TrimSpace(newStartRFC))
	if err != nil {
		return newStartRFC
	}
	lead := DefaultLead
	if os, err1 := time.Parse(time.RFC3339, strings.TrimSpace(oldStartRFC)); err1 == nil {
		if on, err2 := time.Parse(time.RFC3339, strings.TrimSpace(oldNextAtRFC)); err2 == nil {
			if d := os.Sub(on); d >= MinLead {
				lead = d
			}
		}
	}
	return ns.Add(-lead).Format(time.RFC3339)
}

// parseDur parses common duration phrasings ("30m", "1h30m", "45 min", "2 hours").
func parseDur(s string) (time.Duration, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "-" || s == "none" {
		return 0, false
	}
	// Go's own format first (30m, 1h, 1h30m), tolerating internal spaces.
	if d, err := time.ParseDuration(strings.ReplaceAll(s, " ", "")); err == nil && d > 0 {
		return d, true
	}
	// Fall back to "<n> <unit>" phrasings.
	fields := strings.Fields(s)
	if len(fields) >= 2 {
		if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
			unit := fields[1]
			switch {
			case strings.HasPrefix(unit, "min"):
				return time.Duration(n) * time.Minute, true
			case strings.HasPrefix(unit, "hour"), unit == "h", unit == "hr", unit == "hrs":
				return time.Duration(n) * time.Hour, true
			case strings.HasPrefix(unit, "day"):
				return time.Duration(n) * 24 * time.Hour, true
			}
		}
	}
	return 0, false
}

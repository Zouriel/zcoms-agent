package reminders

import (
	"regexp"
	"strings"
)

// Requester is who asked for a reminder — the bridge fills it from the inbound
// sender's session; the agent.sock/CLI path fills it as the owner. It carries
// both the §6 trust identity (Handle/Owner) and the reply address used when the
// requester is also the reminded party ("remind me …").
type Requester struct {
	Transport string // "telegram" | "whatsapp"
	Handle    string // allow-list handle / username (owner + allowlist checks)
	Address   string // native reply address (Telegram chat-id string / WhatsApp jid)
	Name      string // display label
	Owner     bool   // requester is the owner (always true on the agent.sock path)
}

// key returns the requester's "transport|handle" identity stored on the reminder.
func (r Requester) key() string {
	return r.Transport + "|" + strings.TrimSpace(r.Handle)
}

// remindRe strips a leading "remind"/"reminder" verb (so both the bridge prefix
// and a bare command line parse the same).
var remindRe = regexp.MustCompile(`(?i)^\s*remind(?:er)?s?\b\s*`)

// sepRe splits "<who> to|about <task>" on the first " to " / " about " word.
var sepRe = regexp.MustCompile(`(?i)\s+(?:to|about)\s+`)

// selfWords are the <who> values that mean "the requester themselves".
var selfWords = map[string]bool{"me": true, "myself": true, "i": true, "self": true}

// ParseRemind splits a `remind <who> to <task>` command into the target name and
// the free-text task. ok=false when there's no task. When no " to "/" about "
// separator is present, the first word is taken as <who> and the rest as the task
// (so "remind me water the plants" still works).
func ParseRemind(text string) (who, task string, ok bool) {
	body := strings.TrimSpace(remindRe.ReplaceAllString(text, ""))
	if body == "" {
		return "", "", false
	}
	if loc := sepRe.FindStringIndex(body); loc != nil {
		who = strings.TrimSpace(body[:loc[0]])
		task = strings.TrimSpace(body[loc[1]:])
	} else {
		fields := strings.Fields(body)
		who = fields[0]
		task = strings.TrimSpace(strings.TrimPrefix(body, fields[0]))
	}
	if who == "" || task == "" {
		return "", "", false
	}
	return who, task, true
}

// isSelf reports whether a <who> refers to the requester.
func isSelf(who string) bool { return selfWords[norm(who)] }

// thirdPartyRe matches a task that is itself a request to remind someone else:
// "remind Raani to take the report", "please remind my wife about the class".
// <who> is up to three words so "my wife" and "first wife" match, and the task
// is what follows the connecting word.
var thirdPartyRe = regexp.MustCompile(`(?i)^\s*(?:please\s+|can\s+you\s+|could\s+you\s+)*remind(?:er)?\s+(\S+(?:\s+\S+){0,2}?)\s+(?:to|about|that)\s+(.+)$`)

// ThirdParty reports whether a task text is really a request to remind someone
// ELSE, and splits it into that person and the task itself.
//
// It exists because the schedule agents (morning, lineup) can only add events to
// the owner's own calendar: handed "remind Raani to take her father's report"
// they filed it as the owner's own task, and the reminder loop then messaged the
// owner as if he were Raani. Callers use this to route such a request to the
// reminders module instead, where resolveTarget applies the §6 trust gate.
//
// ok is false for the owner's own reminders ("remind me to …"), which belong on
// their own schedule exactly as written.
func ThirdParty(task string) (who, rest string, ok bool) {
	m := thirdPartyRe.FindStringSubmatch(strings.TrimSpace(task))
	if m == nil {
		return "", "", false
	}
	who, rest = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
	if who == "" || rest == "" || isSelf(who) {
		return "", "", false
	}
	// "remind me and Sara to …" is still the owner's own task; only a clean
	// someone-else target should leave their calendar.
	for _, w := range strings.Fields(who) {
		if isSelf(w) {
			return "", "", false
		}
	}
	return who, rest, true
}

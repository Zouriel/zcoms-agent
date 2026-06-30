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

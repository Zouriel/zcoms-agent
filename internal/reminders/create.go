package reminders

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

const remindUsage = "Usage: remind <me|contact name> to <task>  (e.g. \"remind me to water the plants\", \"remind Sara to send the invoice\"). Also: remind list · remind cancel <id>."

// target is the resolved reminded party.
type target struct {
	transport string
	addr      string // native reply address (chat-id string / jid)
	contactID int64
	name      string
	isSelf    bool
}

// HandleCommand runs a `remind …` line from a requester and returns the reply to
// send back to them. The single entry point for both the bridge (allow-listed
// user) and the agent.sock/CLI (owner) paths.
func (d *Comp) HandleCommand(req Requester, text string) string {
	body := strings.TrimSpace(remindRe.ReplaceAllString(text, ""))
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return remindUsage
	}
	switch norm(fields[0]) {
	case "help", "?":
		return remindUsage
	case "list", "ls":
		return d.listReply(req)
	case "cancel", "rm", "stop":
		return d.cancelReply(req, fields[1:])
	case "settings", "config", "set":
		return d.settingsReply(req, fields[1:])
	}
	return d.createReply(req, text)
}

// createReply parses, resolves the target, enforces §6, classifies, persists, and
// schedules the first tick — returning the confirmation (or a clear rejection).
func (d *Comp) createReply(req Requester, text string) string {
	who, task, ok := ParseRemind(text)
	if !ok {
		return remindUsage
	}
	tgt, err := d.resolveTarget(req, who)
	if err != nil {
		return "⚠️ " + err.Error()
	}

	cfg := d.cfg()
	if !cfg.Enabled {
		return "🔕 Reminders are turned off right now. Switch them back on in the console settings."
	}
	now := time.Now()
	dec := proportionate(applyConfig(d.classify.Classify(task, now), cfg))
	state, nextAt := firstFire(dec, now)

	r := store.Reminder{
		RequesterAddr:   req.key(),
		RequesterName:   req.Name,
		TargetContactID: tgt.contactID,
		TargetTransport: tgt.transport,
		TargetAddr:      tgt.addr,
		TargetName:      tgt.name,
		TaskText:        task,
		Kind:            dec.Kind,
		RecurSpec:       dec.RecurSpec,
		DeadlineBound:   dec.DeadlineBound,
		EventAt:         rfc(dec.EventAt),
		PreDelaySecs:    int(dec.PreDelay / time.Second),
		PostGapSecs:     int(dec.PostGap / time.Second),
		State:           state,
		NextAt:          rfc(nextAt),
	}
	saved, err := d.store.CreateReminder(r)
	if err != nil {
		return "⚠️ couldn't save the reminder: " + err.Error()
	}
	d.store.AddReminderEvent(saved.ID, "create", task)
	d.log.Printf("created #%d for %s (%s) next=%s", saved.ID, tgt.name, dec.Kind, rfc(nextAt))

	who2 := "you"
	if !tgt.isSelf {
		who2 = tgt.name
	}
	return fmt.Sprintf("✅ Reminder #%d set. I'll remind %s to %s, first nudge %s.",
		saved.ID, who2, task, humanWhen(nextAt, now))
}

// resolveTarget resolves <who> to a reachable address and enforces the §6 trust
// rule: the owner may remind anyone in contacts; a non-owner allow-listed
// requester may only target other allow-listed people. Enforced once, here,
// before anything is persisted.
func (d *Comp) resolveTarget(req Requester, who string) (target, error) {
	if isSelf(who) {
		addr := strings.TrimSpace(req.Address)
		tp := req.Transport
		if addr == "" {
			// agent.sock/CLI owner with an unresolved main chat.
			if _, oc := d.owner(); oc != 0 {
				tp, addr = "telegram", itoa(oc)
			}
		}
		if addr == "" {
			return target{}, fmt.Errorf("I don't know how to reach you to remind you. Set your main_user/Telegram first")
		}
		name := req.Name
		if name == "" {
			name = "you"
		}
		return target{transport: tp, addr: addr, name: name, isSelf: true}, nil
	}

	matches, err := d.client.ResolveContact(who)
	if err != nil {
		return target{}, fmt.Errorf("couldn't reach the contacts directory: %v", err)
	}
	if len(matches) == 0 {
		return target{}, fmt.Errorf("I don't have a contact named %q. Add them with `zc contacts add`", who)
	}
	c := matches[0]

	// §6 trust: non-owner requesters may only target allow-listed people.
	if !req.Owner && !d.contactAllowListed(c) {
		return target{}, fmt.Errorf("you can only set reminders for allow-listed people, and %s isn't on the allowlist", c.Name)
	}

	tp, addr, err := d.reachable(c)
	if err != nil {
		return target{}, err
	}
	return target{transport: tp, addr: addr, contactID: c.ID, name: c.Name}, nil
}

// reachable picks the transport+address to reach a contact, preferring Telegram,
// then WhatsApp (the two routed transports today). Telegram is resolved to a chat
// id so inbound replies (keyed by chat id) match.
func (d *Comp) reachable(c client.Contact) (transport, addr string, err error) {
	if h := strings.TrimSpace(c.Address("telegram")); h != "" {
		id, rerr := d.client.Resolve(h)
		if rerr != nil {
			return "", "", fmt.Errorf("couldn't resolve %s on Telegram: %v", c.Name, rerr)
		}
		return "telegram", itoa(id), nil
	}
	if h := strings.TrimSpace(c.Address("whatsapp")); h != "" {
		return "whatsapp", runner.WADigits(h) + "@s.whatsapp.net", nil
	}
	return "", "", fmt.Errorf("%s has no Telegram or WhatsApp address I can reach", c.Name)
}

// contactAllowListed reports whether any of a contact's channels is allow-listed.
func (d *Comp) contactAllowListed(c client.Contact) bool {
	set := d.allowSet()
	for _, p := range []string{"telegram", "whatsapp", "instagram", "discord", "viber"} {
		if a := strings.TrimSpace(c.Address(p)); a != "" && set[runner.AllowKey(p, a)] {
			return true
		}
	}
	return false
}

// settingsReply shows or sets a reminder config knob (owner only). The change is
// written to agent.db and read live by the engine — no restart.
func (d *Comp) settingsReply(req Requester, args []string) string {
	if !req.Owner {
		return "⚠️ only the owner can change reminder settings"
	}
	c := d.cfg()
	if len(args) == 0 {
		return fmt.Sprintf("Reminders: enabled=%v, voice=%s, first_nudge=%dm, followup=%dm, deadline_lead=%dm, deadline_after=%dm, max_nudges=%d",
			c.Enabled, c.Voice, c.FirstNudgeMins, c.FollowupMins, c.DeadlineLeadMins, c.DeadlineAfterMins, c.MaxNudges)
	}
	if len(args) < 2 {
		return "Usage: remind settings <enabled|voice|first_nudge_mins|followup_mins|deadline_lead_mins|deadline_after_mins|max_nudges> <value>"
	}
	key := SettingKey(args[0])
	if key == "" {
		return "⚠️ unknown reminder setting " + args[0]
	}
	val := strings.TrimSpace(strings.Join(args[1:], " "))
	if err := d.store.SetSetting(store.Owner, key, val); err != nil {
		return "⚠️ " + err.Error()
	}
	return "✅ " + args[0] + " = " + val
}

// listReply lists the requester's in-flight reminders (the owner sees all).
func (d *Comp) listReply(req Requester) string {
	rs, err := d.store.ActiveReminders()
	if err != nil {
		return "⚠️ couldn't list reminders: " + err.Error()
	}
	var b strings.Builder
	n := 0
	for _, r := range rs {
		if !req.Owner && r.RequesterAddr != req.key() {
			continue
		}
		who := r.TargetName
		if who == "" {
			who = r.TargetAddr
		}
		fmt.Fprintf(&b, "#%d → %s: %s  [%s]\n", r.ID, who, r.TaskText, r.State)
		n++
	}
	if n == 0 {
		return "No active reminders."
	}
	return strings.TrimRight(b.String(), "\n")
}

// cancelReply cancels a reminder if the requester owns it (owner, the requester
// who set it, or the reminded party opting out).
func (d *Comp) cancelReply(req Requester, args []string) string {
	if len(args) == 0 {
		return "Usage: remind cancel <id>"
	}
	id, perr := strconv.ParseInt(args[0], 10, 64)
	if perr != nil {
		return "⚠️ bad reminder id " + args[0]
	}
	r, ok, err := d.store.GetReminder(id)
	if err != nil {
		return "⚠️ " + err.Error()
	}
	if !ok {
		return fmt.Sprintf("No reminder #%d.", id)
	}
	if !req.Owner && r.RequesterAddr != req.key() && r.TargetAddr != req.Address {
		return "⚠️ that reminder isn't yours to cancel"
	}
	if err := d.store.CancelReminder(id); err != nil {
		return "⚠️ " + err.Error()
	}
	d.store.AddReminderEvent(id, "cancel", "")
	return fmt.Sprintf("🗑️ Reminder #%d cancelled.", id)
}

// --- timing helpers ----------------------------------------------------------

// applyConfig folds the owner's tunable defaults into a fresh Decision: a deadline
// event uses the configured lead/after, an open (non-explicit) task uses the
// configured first-nudge + follow-up gaps, and an explicitly-timed task keeps its
// own inferred lead but takes the configured follow-up gap. Recurring timing is
// event-anchored and left alone.
func applyConfig(d Decision, c Config) Decision {
	switch {
	case d.Kind == "recurring":
		// leave
	case d.DeadlineBound:
		// Config lead is a FLOOR, not an override: a prep/travel task the model
		// gave a longer lead ("get ready" → 45m) keeps it, but every deadline gets
		// at least the configured lead so it never fires right at the event.
		if c.deadlineLead() > d.PreDelay {
			d.PreDelay = c.deadlineLead()
		}
		if d.PostGap <= 0 {
			d.PostGap = c.deadlineAfter()
		}
	case !d.Explicit:
		d.PreDelay = c.firstNudge()
		d.PostGap = c.followup()
	default:
		d.PostGap = c.followup()
	}
	return d
}

// proportionate keeps the follow-up gap sensible relative to how soon the task
// is due: a near-term task ("eat in 2 minutes") shouldn't wait the default 15 min
// to be checked on. Deadline events are event-anchored, so they're left alone.
func proportionate(d Decision) Decision {
	if d.DeadlineBound || d.Kind == "recurring" {
		return d
	}
	if d.PreDelay > 0 && d.PreDelay < 15*time.Minute {
		if cap := d.PreDelay + 3*time.Minute; d.PostGap > cap {
			d.PostGap = cap
		}
	}
	if d.PostGap < 2*time.Minute {
		d.PostGap = 2 * time.Minute
	}
	return d
}

// firstFire computes the initial state + next tick from a creation Decision.
func firstFire(d Decision, now time.Time) (state string, at time.Time) {
	switch {
	case d.Kind == "recurring" && !d.EventAt.IsZero():
		return store.ReminderScheduled, d.EventAt
	case d.DeadlineBound && !d.EventAt.IsZero():
		at = d.EventAt.Add(-d.PreDelay)
		if !at.After(now) {
			at = now.Add(30 * time.Second) // event is imminent — nudge right away
		}
		return store.ReminderScheduled, at
	case !d.EventAt.IsZero():
		// Explicit act-at time, not a deadline event: pre-remind at the time.
		return store.ReminderScheduled, d.EventAt
	default:
		return store.ReminderScheduled, now.Add(d.PreDelay)
	}
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// humanWhen renders an upcoming instant relative to now for a confirmation line.
func humanWhen(t, now time.Time) string {
	if t.IsZero() {
		return "shortly"
	}
	d := t.Sub(now)
	switch {
	case d < time.Minute:
		return "in under a minute"
	case d < time.Hour:
		return fmt.Sprintf("in about %d min", int(d.Minutes()+0.5))
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "at " + t.Format("3:04 PM")
	default:
		return "on " + t.Format("Mon Jan 2 at 3:04 PM")
	}
}

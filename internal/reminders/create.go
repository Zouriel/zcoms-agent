package reminders

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

const remindUsage = "Usage: remind <me|contact name> to <task>  (e.g. \"remind me to water the plants\", \"remind Sara to send the invoice\"). Also: remind list · remind cancel <id>."

// target is the resolved reminded party.
type target struct {
	transport string
	addr      string
	contactID int64
	name      string
	isSelf    bool
}

// HandleCommand runs a `remind …` line and returns the reply to the requester.
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

// createReply parses, resolves the recipient, enforces §6, and persists an active
// reminder due now — the first run is a planning run where the agent decides the
// real timing.
func (d *Comp) createReply(req Requester, text string) string {
	who, task, ok := d.interpretRemind(req, text)
	if !ok {
		return remindUsage
	}
	if !d.cfg().Enabled {
		return "🔕 Reminders are turned off right now. Switch them back on in the console settings."
	}
	tgt, err := d.resolveTarget(req, who)
	if err != nil {
		return "⚠️ " + err.Error()
	}

	// The agent plans the first reach-out time at registration, so lead time (e.g.
	// nudge before you have to get ready) is decided up front, not on a later run.
	now := time.Now()
	at, note := d.planFirst(store.Reminder{
		Task: task, FromName: req.Name, RecipientName: tgt.name, RecipientContactID: tgt.contactID,
	}, now)

	saved, err := d.store.CreateReminder(store.Reminder{
		FromAddr: req.key(), FromName: req.Name,
		RecipientTransport: tgt.transport, RecipientAddr: tgt.addr,
		RecipientName: tgt.name, RecipientContactID: tgt.contactID,
		Task: task, State: store.ReminderActive, NextAt: rfc(at), CarryOver: note,
	})
	if err != nil {
		return "⚠️ couldn't save the reminder: " + err.Error()
	}
	d.store.AddReminderEvent(saved.ID, "create", task)
	if note != "" {
		d.store.AddReminderEvent(saved.ID, "note", note)
	}
	d.log.Printf("created #%d for %s: %s (first at %s)", saved.ID, tgt.name, task, rfc(at))

	who2 := "you"
	if !tgt.isSelf {
		who2 = tgt.name
	}
	return fmt.Sprintf("✅ Reminder #%d set. I'll remind %s to %s%s.", saved.ID, who2, task, whenClause(at, now))
}

// interpretRemind reads a natural-language reminder request through the reminder
// agent and pulls out who it's for and what the task is, so everyday phrasings
// work — e.g. "remind Zouriel that he has to come pick me up at 11:45", which the
// old regex split on the first " to " and mis-took the name as "Zouriel that he
// has". It falls back to the regex ParseRemind when there's no backend, the turn
// errors, or it comes back empty, so the command still works headless and in tests.
// Only text extraction is delegated: the §6 trust gate stays deterministic in
// resolveTarget, which never trusts this output for access.
func (d *Comp) interpretRemind(req Requester, text string) (who, task string, ok bool) {
	if d.turn == nil {
		return ParseRemind(text)
	}
	seed := ""
	if d.seed != nil {
		seed = d.seed(personas.Reminders)
	}
	out, _, err := d.turn(withSeed(seed, interpretPrompt(req, text)), "")
	if err != nil {
		d.log.Printf("interpret: %v", err)
		return ParseRemind(text)
	}
	f := parseFields(out)
	who, task = strings.TrimSpace(f["who"]), strings.TrimSpace(f["task"])
	if who == "" || task == "" {
		return ParseRemind(text)
	}
	return who, task, true
}

// interpretPrompt asks the agent to read the raw request and return just the
// target and the task on two fixed lines (parsed by parseFields).
func interpretPrompt(req Requester, text string) string {
	me := strings.TrimSpace(req.Name)
	if me == "" {
		me = "the person talking to you"
	}
	return strings.Join([]string{
		"Someone sent you a request to set a reminder. Read it and pull out two things: WHO the reminder is for, and WHAT they need to be reminded of.",
		"Their exact message: " + quote(text),
		"The person asking is " + me + ". If the reminder is for themselves, WHO is the single word me.",
		"",
		"Rules:",
		"- WHO is only the person's name as they referred to them (a first name is fine), or the single word me for the asker. Never fold connecting words (\"that\", \"to\") or any of the task into WHO.",
		"- TASK is what needs doing, phrased so it reads naturally right after the word \"to\" (for example \"come pick me up at 11:45\", \"send the invoice\"). Keep any time or place they mentioned.",
		"Reply EXACTLY in these two lines and nothing else:",
		"WHO: the name, or me",
		"TASK: the task",
	}, "\n")
}

// whenClause renders the planned first reach-out time for the confirmation line.
func whenClause(at, now time.Time) string {
	if !at.After(now.Add(90 * time.Second)) {
		return ", starting now"
	}
	if at.YearDay() == now.YearDay() && at.Year() == now.Year() {
		return ", starting around " + at.Local().Format("3:04 PM")
	}
	return ", starting " + at.Local().Format("Mon Jan 2 at 3:04 PM")
}

// resolveTarget resolves <who> to a reachable recipient and enforces §6: the owner
// may remind anyone in contacts; a non-owner allow-listed requester may only target
// other allow-listed people. Enforced once, here, before anything is persisted.
func (d *Comp) resolveTarget(req Requester, who string) (target, error) {
	if isSelf(who) {
		addr, tp := strings.TrimSpace(req.Address), req.Transport
		if addr == "" {
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
	if !req.Owner && !d.contactAllowListed(c) {
		return target{}, fmt.Errorf("you can only set reminders for allow-listed people, and %s isn't on the allowlist", c.Name)
	}
	tp, addr, err := d.reachable(c)
	if err != nil {
		return target{}, err
	}
	return target{transport: tp, addr: addr, contactID: c.ID, name: c.Name}, nil
}

// reachable picks the transport + address to reach a contact, preferring Telegram
// (resolved to a chat id so inbound replies match), then WhatsApp.
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

// listReply lists the requester's active reminders (the owner sees all).
func (d *Comp) listReply(req Requester) string {
	rs, err := d.store.ActiveReminders()
	if err != nil {
		return "⚠️ couldn't list reminders: " + err.Error()
	}
	var b strings.Builder
	n := 0
	for _, r := range rs {
		if !req.Owner && r.FromAddr != req.key() {
			continue
		}
		who := r.RecipientName
		if who == "" {
			who = "you"
		}
		fmt.Fprintf(&b, "#%d → %s: %s\n", r.ID, who, r.Task)
		if r.CarryOver != "" {
			fmt.Fprintf(&b, "      note: %s\n", r.CarryOver)
		}
		n++
	}
	if n == 0 {
		return "No active reminders."
	}
	return strings.TrimRight(b.String(), "\n")
}

// cancelReply cancels a reminder the requester owns (owner, the setter, or the
// reminded party opting out).
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
	if !req.Owner && r.FromAddr != req.key() && r.RecipientAddr != req.Address {
		return "⚠️ that reminder isn't yours to cancel"
	}
	if err := d.store.CancelReminder(id); err != nil {
		return "⚠️ " + err.Error()
	}
	d.store.AddReminderEvent(id, "cancel", "")
	return fmt.Sprintf("🗑️ Reminder #%d cancelled.", id)
}

// settingsReply shows or sets a reminder config knob (owner only), read live.
func (d *Comp) settingsReply(req Requester, args []string) string {
	if !req.Owner {
		return "⚠️ only the owner can change reminder settings"
	}
	c := d.cfg()
	if len(args) == 0 {
		return fmt.Sprintf("Reminders: enabled=%v, max_runs=%d, reply_wait=%dm", c.Enabled, c.MaxRuns, c.ReplyWaitMins)
	}
	if len(args) < 2 {
		return "Usage: remind settings <enabled|max_runs|reply_wait_mins> <value>"
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

package reminders

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/eventtime"
	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

const remindUsage = "Usage: remind <me|contact name> to <task>  (e.g. \"remind me to water the plants\", \"remind Sara to send the invoice\"). Also: remind list · remind edit <id> <task|when|note> <value> · remind cancel <id>."

const editUsage = "Usage: remind edit <id> task <new task> · remind edit <id> when <new time> · remind edit <id> note <text>"

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
		all := len(fields) > 1 && (norm(fields[1]) == "all" || norm(fields[1]) == "finished")
		return d.listReply(req, all)
	case "cancel", "rm", "stop", "delete", "del", "remove":
		return d.cancelReply(req, fields[1:])
	case "edit", "change", "update":
		return d.editReply(req, fields[1:])
	case "settings", "config", "set":
		return d.settingsReply(req, fields[1:])
	}
	return d.createReply(req, text)
}

// createReply parses, resolves the recipient, enforces §6, and persists an active
// reminder due now — the first run is a planning run where the agent decides the
// real timing.
func (d *Comp) createReply(req Requester, text string) string {
	// Name the asker as the contacts directory knows them, not by the handle they
	// happen to message from: it goes into the task text, the confirmation, and
	// every later message about this reminder.
	req.Name = d.displayName(req)
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
		"- TASK is what needs doing, phrased so it reads naturally right after the word \"to\" (for example \"send the invoice\"). Keep any time or place they mentioned.",
		"- Write TASK from the point of view of the PERSON BEING REMINDED, not the asker: where the asker said \"me\", \"my\" or \"I\" about themselves, name them instead. " + me + " asking you to remind someone \"to come pick me up at 11:45\" becomes \"come pick up " + me + " at 11:45\", because that reminder is read by somebody else.",
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
	for _, p := range runner.AllowPlatforms {
		if a := strings.TrimSpace(c.Address(p)); a != "" && set[runner.AllowKey(p, a)] {
			return true
		}
	}
	return false
}

// listReply lists the requester's active reminders (the owner sees all), each
// with the times that make it actionable: when it next fires, when the event it
// is about starts, and when it was set. Without those a list of bare task lines
// gives no way to tell a reminder due tonight from one that went past weeks ago.
func (d *Comp) listReply(req Requester, all bool) string {
	list := d.store.ActiveReminders
	if all {
		list = d.store.ListReminders
	}
	rs, err := list()
	if err != nil {
		return "⚠️ couldn't list reminders: " + err.Error()
	}
	now := time.Now()
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
		state := ""
		if r.State != store.ReminderActive {
			state = "  [" + r.State + "]"
		}
		fmt.Fprintf(&b, "#%d → %s: %s%s\n", r.ID, who, r.Task, state)
		if meta := reminderTimes(r, now); meta != "" {
			fmt.Fprintf(&b, "      %s\n", meta)
		}
		if r.CarryOver != "" {
			fmt.Fprintf(&b, "      note: %s\n", r.CarryOver)
		}
		n++
	}
	if n == 0 {
		if all {
			return "No reminders at all."
		}
		return "No active reminders."
	}
	return strings.TrimRight(b.String(), "\n")
}

// reminderTimes renders one reminder's timing line: when it next fires (flagged
// when that moment has already passed), the event window it belongs to, when it
// was set, and how many times it has run.
func reminderTimes(r store.Reminder, now time.Time) string {
	loc := now.Location()
	var parts []string

	if next := parseInstant(r.NextAt, loc); !next.IsZero() {
		part := "due " + stamp(next, now)
		if next.Before(now) {
			part += " (overdue by " + humanSince(now.Sub(next)) + ")"
		}
		parts = append(parts, part)
	} else {
		parts = append(parts, "no time set")
	}

	if start := parseInstant(r.EventStart, loc); !start.IsZero() {
		event := "event " + stamp(start, now)
		if end := parseInstant(r.EventEnd, loc); !end.IsZero() && end.After(start) {
			event += " to " + end.Format("3:04 PM")
		}
		parts = append(parts, event)
	}

	if set := parseInstant(r.CreatedAt, loc); !set.IsZero() {
		parts = append(parts, "set "+stamp(set, now))
	}

	if r.Runs > 0 {
		parts = append(parts, fmt.Sprintf("%d run(s)", r.Runs))
	}

	return strings.Join(parts, " · ")
}

// stamp writes a moment the way a person reads one, dropping the year when it is
// the current one and the date when it is today.
func stamp(t, now time.Time) string {
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "today " + t.Format("3:04 PM")
	case t.Year() == now.Year():
		return t.Format("Mon 2 Jan, 3:04 PM")
	default:
		return t.Format("Mon 2 Jan 2006, 3:04 PM")
	}
}

// humanSince renders an elapsed span in the largest unit that still reads plainly.
func humanSince(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
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

// editReply changes an existing reminder in place: its task text, when it fires,
// or its carry-over note. It applies the same trust gate as cancelReply, so a
// reminder can only be edited by the owner, the person who set it, or the person
// being reminded.
//
// Retargeting is deliberately not editable. Who a reminder reaches is the one
// field the §6 gate is built around, and moving it is a create, not an edit.
func (d *Comp) editReply(req Requester, args []string) string {
	if len(args) < 3 {
		return editUsage
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
		return "⚠️ that reminder isn't yours to edit"
	}
	if r.State == store.ReminderCancelled {
		return fmt.Sprintf("⚠️ reminder #%d was cancelled; set a new one instead.", id)
	}

	field, value := norm(args[1]), strings.TrimSpace(strings.Join(args[2:], " "))
	if value == "" {
		return editUsage
	}

	now := time.Now()
	var changed string
	switch field {
	case "task", "text", "what":
		// task_text has its own setter: the per-run UpdateReminder deliberately
		// cannot touch it, so the reminder agent can never rewrite its own brief.
		if err := d.store.SetReminderTask(id, value); err != nil {
			return "⚠️ couldn't save the change: " + err.Error()
		}
		d.store.AddReminderEvent(id, "edit", "task: "+value)
		d.log.Printf("edited #%d task", id)
		return fmt.Sprintf("✏️ Reminder #%d updated, task is now: %s.", id, value)
	case "note", "carryover", "carry_over":
		r.CarryOver = value
		changed = "note is now: " + value
	case "when", "time", "at", "due":
		at, verr := d.editedTime(r, value, now)
		if verr != nil {
			return "⚠️ " + verr.Error()
		}
		// An event-backed reminder keeps its lead: moving the event moves the
		// nudge with it rather than collapsing the two onto the same moment.
		if strings.TrimSpace(r.EventStart) != "" {
			r.NextAt = eventtime.Shift(r.EventStart, r.NextAt, rfc(at))
			r.EventStart = rfc(at)
		} else {
			r.NextAt = rfc(at)
		}
		// A reminder that already finished is live again once it is given a
		// future time, which is what "move it to tomorrow" is asking for.
		if r.State == store.ReminderDone && at.After(now) {
			r.State = store.ReminderActive
		}
		changed = "now due " + stamp(at, now)
	default:
		return editUsage
	}

	if err := d.store.UpdateReminder(r); err != nil {
		return "⚠️ couldn't save the change: " + err.Error()
	}
	d.store.AddReminderEvent(id, "edit", field+": "+value)
	d.log.Printf("edited #%d %s", id, field)
	return fmt.Sprintf("✏️ Reminder #%d updated, %s.", id, changed)
}

// editedTime reads the new moment for `remind edit <id> when …`. A bare day word
// ("tomorrow", "friday") carries no clock time, so the reminder keeps the time of
// day it already had rather than silently jumping to midnight.
func (d *Comp) editedTime(r store.Reminder, value string, now time.Time) (time.Time, error) {
	at, hasTime, err := parseAnchor(value, now)
	if err != nil {
		return time.Time{}, err
	}
	if hasTime {
		return at, nil
	}
	loc := now.Location()
	clock := parseInstant(r.EventStart, loc)
	if clock.IsZero() {
		clock = parseInstant(r.NextAt, loc)
	}
	if clock.IsZero() {
		clock = time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, loc)
	}
	return time.Date(at.Year(), at.Month(), at.Day(), clock.Hour(), clock.Minute(), 0, 0, loc), nil
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

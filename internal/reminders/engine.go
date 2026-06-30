package reminders

import (
	"fmt"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// FireDue is the scheduler-tick entry (registered as a 30s Interval). It advances
// every reminder whose next_at has arrived. The DB is the source of truth, so a
// reminder that came due while the agent was down fires on the first tick after
// restart — no in-memory state to rebuild.
func (d *Comp) FireDue() { d.fireDue(time.Now()) }

func (d *Comp) fireDue(now time.Time) {
	if !d.cfg().Enabled {
		return // paused: don't advance reminders (they resume when re-enabled)
	}
	due, err := d.store.DueReminders(rfc(now))
	if err != nil {
		d.log.Printf("due scan: %v", err)
		return
	}
	for _, r := range due {
		d.advance(r, now)
	}
}

// advance runs the tick-driven transition for one reminder (replies are handled
// separately by the matcher). See the §1 state diagram.
func (d *Comp) advance(r store.Reminder, now time.Time) {
	switch r.State {
	case store.ReminderScheduled:
		d.firePreReminder(r, now)
	case store.ReminderPreReminded, store.ReminderSnoozed:
		d.fireConfirm(r, now)
	case store.ReminderAwaiting:
		d.confirmTimeout(r, now)
	}
}

// firePreReminder sends the pre-reminder (the "time for X" nudge). A recurring
// reminder reschedules its next occurrence here; a one-off advances to the
// confirm wait.
func (d *Comp) firePreReminder(r store.Reminder, now time.Time) {
	d.sendTarget(r, d.msg(MsgPre, r, 0, 0))
	d.store.AddReminderEvent(r.ID, "pre_remind", "")
	if r.Kind == "recurring" {
		d.scheduleNextRecur(r, now)
		return
	}
	r.State = store.ReminderPreReminded
	r.NextAt = rfc(d.confirmAt(r, now))
	d.save(r)
}

// fireConfirm asks whether the task is done (or how the event went), then waits
// for a reply.
func (d *Comp) fireConfirm(r store.Reminder, now time.Time) {
	d.sendTarget(r, d.msg(MsgConfirm, r, 0, 0))
	d.store.AddReminderEvent(r.ID, "ask_confirm", "")
	r.State = store.ReminderAwaiting
	r.NextAt = rfc(now.Add(d.reaskGap(r)))
	d.save(r)
}

// confirmTimeout handles a confirm window elapsing with no reply: a passed
// deadline event is missed; an open task is re-asked (bounded).
func (d *Comp) confirmTimeout(r store.Reminder, now time.Time) {
	if r.DeadlineBound && d.eventPassed(r, now) {
		d.markMissed(r, now)
		return
	}
	if r.Attempts >= d.cfg().MaxNudges {
		d.giveUp(r)
		return
	}
	r.Attempts++
	d.sendTarget(r, d.msg(MsgNudge, r, r.Attempts, 0))
	d.store.AddReminderEvent(r.ID, "ask_confirm", "re-ask")
	r.State = store.ReminderAwaiting
	r.NextAt = rfc(now.Add(d.reaskGap(r)))
	d.save(r)
}

// --- reply matcher -----------------------------------------------------------

// Owns reports whether an engaged reminder is waiting on a reply from a Telegram
// chat, so the agent's event router sends that message here instead of the bridge.
func (d *Comp) Owns(chatID int64) bool {
	_, ok, _ := d.store.OpenReminderForTarget("telegram", itoa(chatID))
	return ok
}

// OwnsWA is Owns for a WhatsApp jid.
func (d *Comp) OwnsWA(jid string) bool {
	_, ok, _ := d.store.OpenReminderForTarget("whatsapp", jid)
	return ok
}

// FeedTelegram routes a Telegram reply into its open reminder. Returns true when
// consumed.
func (d *Comp) FeedTelegram(chatID int64, text string) bool {
	return d.feed("telegram", itoa(chatID), "", text)
}

// FeedWhatsApp routes a WhatsApp reply into its open reminder, clearing the
// daemon's unread (like errands) so triage doesn't also digest it.
func (d *Comp) FeedWhatsApp(jid, msgRef, text string) bool {
	consumed := d.feed("whatsapp", jid, msgRef, text)
	if consumed && msgRef != "" {
		_ = d.client.MarkReadOn("whatsapp", jid, []string{msgRef})
	}
	return consumed
}

// feed correlates an inbound reply to the open reminder for (transport, addr) and
// advances it: a positive reply closes (or reschedules a recurring) it; a
// negative one snoozes (until-done) or reports a missed deadline.
func (d *Comp) feed(transport, addr, _ /*msgRef*/, text string) bool {
	r, ok, err := d.store.OpenReminderForTarget(transport, addr)
	if err != nil || !ok {
		return false
	}
	now := time.Now()
	d.store.AddReminderEvent(r.ID, "reply", text)
	r.LastReply = text

	if isOptOut(text) {
		_ = d.store.CancelReminder(r.ID)
		d.store.AddReminderEvent(r.ID, "cancel", "opt-out")
		d.sendTarget(r, "👍 Okay, I won't remind you about that again.")
		return true
	}

	verdict := d.classify.ClassifyReply(r.TaskText, text)
	switch {
	case verdict.Positive:
		d.onConfirmed(r, now)
	case verdict.Ack:
		d.onAck(r) // heard, not done — keep the schedule, don't close or chase
	default:
		d.onNegative(r, verdict, now)
	}
	return true
}

// onAck records a bare acknowledgment ("ok", "will do") without advancing the
// loop: the reminder keeps its current state + next tick and checks in as planned.
func (d *Comp) onAck(r store.Reminder) {
	d.store.AddReminderEvent(r.ID, "ack", r.LastReply)
	d.save(r) // persist last_reply; state/next_at unchanged
}

func (d *Comp) onConfirmed(r store.Reminder, now time.Time) {
	d.sendTarget(r, d.msg(MsgDone, r, 0, 0))
	d.store.AddReminderEvent(r.ID, "done", "")
	if r.Kind == "recurring" {
		d.scheduleNextRecur(r, now)
	} else {
		r.State = store.ReminderDone
		r.NextAt = ""
		d.save(r)
	}
	d.reportToRequester(r, fmt.Sprintf("✅ %s confirmed: \"%s\" is done.", d.who(r), r.TaskText))
}

func (d *Comp) onNegative(r store.Reminder, verdict ReplyVerdict, now time.Time) {
	if r.DeadlineBound && d.eventPassed(r, now) {
		d.markMissed(r, now)
		return
	}
	if r.Attempts >= d.cfg().MaxNudges {
		d.giveUp(r)
		return
	}
	r.Attempts++
	gap := verdict.NewGap
	if gap <= 0 {
		gap = d.reaskGap(r)
	}
	d.sendTarget(r, d.msgGap(MsgSnoozeAck, r, humanDur(gap)))
	d.store.AddReminderEvent(r.ID, "snooze", humanDur(gap))
	r.State = store.ReminderSnoozed
	r.NextAt = rfc(now.Add(gap))
	d.save(r)
}

func (d *Comp) markMissed(r store.Reminder, now time.Time) {
	d.sendTarget(r, d.msg(MsgMissed, r, 0, 0))
	d.store.AddReminderEvent(r.ID, "missed", "")
	r.State = store.ReminderMissed
	r.NextAt = ""
	d.save(r)
	d.reportToRequester(r, fmt.Sprintf("⏰ Heads up: %s hasn't gotten to \"%s\" yet. Want to follow up?", d.who(r), r.TaskText))
}

func (d *Comp) giveUp(r store.Reminder) {
	d.sendTarget(r, fmt.Sprintf("I'll stop nudging about \"%s\". Tell me if you still need it.", r.TaskText))
	r.State = store.ReminderCancelled
	r.NextAt = ""
	d.save(r)
}

func (d *Comp) scheduleNextRecur(r store.Reminder, now time.Time) {
	next, ok := nextRecur(r.RecurSpec, now)
	if !ok {
		r.State = store.ReminderDone // unparseable spec — stop rather than loop blindly
		r.NextAt = ""
		d.save(r)
		return
	}
	r.EventAt = rfc(next)
	r.State = store.ReminderScheduled
	r.NextAt = rfc(next)
	d.save(r)
}

// reportToRequester sends the requester a closing note for a third-party reminder
// (self reminders have no separate requester to tell).
func (d *Comp) reportToRequester(r store.Reminder, msg string) {
	if r.TargetContactID == 0 {
		return // self reminder — requester is the reminded party
	}
	parts := strings.SplitN(r.RequesterAddr, "|", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
		return
	}
	_ = d.sendTo(parts[0], parts[1], msg)
}

// --- helpers -----------------------------------------------------------------

func (d *Comp) save(r store.Reminder) {
	if err := d.store.UpdateReminder(r); err != nil {
		d.log.Printf("update #%d: %v", r.ID, err)
	}
}

func (d *Comp) sendTarget(r store.Reminder, text string) {
	if err := d.sendTo(r.TargetTransport, r.TargetAddr, text); err != nil {
		d.log.Printf("send #%d → %s: %v", r.ID, r.TargetAddr, err)
	}
}

// confirmAt is when to ask "did you do it?" — after the event for a deadline
// reminder (so we ask how it went), else one post-gap after the pre-reminder.
func (d *Comp) confirmAt(r store.Reminder, now time.Time) time.Time {
	gap := d.reaskGap(r)
	if r.DeadlineBound && r.EventAt != "" {
		if ev, err := time.Parse(time.RFC3339, r.EventAt); err == nil {
			if t := ev.Add(gap); t.After(now) {
				return t
			}
		}
	}
	return now.Add(gap)
}

func (d *Comp) reaskGap(r store.Reminder) time.Duration {
	if r.PostGapSecs > 0 {
		return time.Duration(r.PostGapSecs) * time.Second
	}
	return d.cfg().followup()
}

func (d *Comp) eventPassed(r store.Reminder, now time.Time) bool {
	if r.EventAt == "" {
		return true // deadline-bound but no concrete time — treat the window as closed
	}
	ev, err := time.Parse(time.RFC3339, r.EventAt)
	return err != nil || now.After(ev)
}

// who is how the reminded party is named in a report to the requester.
func (d *Comp) who(r store.Reminder) string {
	if r.TargetName != "" {
		return r.TargetName
	}
	return "they"
}

// msg writes one humane message for a loop beat: the agent voice (composer) when
// wired, else the deterministic template. attempt drives motivation escalation on
// re-nudges.
func (d *Comp) msg(kind MsgKind, r store.Reminder, attempt int, _ time.Duration) string {
	return d.msgGap(kind, withAttempt(r, attempt), "")
}

func (d *Comp) msgGap(kind MsgKind, r store.Reminder, gap string) string {
	ctx := d.ctxFor(r, gap)
	// The "simple" voice setting (or no composer) uses the deterministic templates.
	if d.composer != nil && d.cfg().agentVoice() {
		if s := d.composer.Compose(kind, ctx); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return templateLine(kind, ctx)
}

// withAttempt stamps the re-ask count onto a copy so ctxFor can read it.
func withAttempt(r store.Reminder, attempt int) store.Reminder {
	r.Attempts = attempt
	return r
}

func (d *Comp) ctxFor(r store.Reminder, gap string) ComposeCtx {
	self := r.TargetContactID == 0
	name := r.TargetName
	if self {
		name = ""
	}
	ev := ""
	if r.EventAt != "" {
		if t, err := time.Parse(time.RFC3339, r.EventAt); err == nil {
			ev = t.Local().Format("Mon 3:04 PM")
		}
	}
	return ComposeCtx{
		Task: r.TaskText, TargetName: name, Self: self,
		DeadlineBound: r.DeadlineBound, EventLocal: ev, Attempt: r.Attempts, Gap: gap,
	}
}

// optOutRe matches a reply that opts a reminder out entirely.
func isOptOut(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "stop", "cancel", "stop reminding me", "unsubscribe", "leave me alone":
		return true
	}
	return false
}

// humanDur renders a duration compactly ("15m", "1h30m", "2h").
func humanDur(d time.Duration) string {
	if d < time.Minute {
		return "a moment"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

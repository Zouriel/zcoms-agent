package reminders

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms-agent/internal/timeexpr"
)

// NewAgentTurn builds the real reminder-agent turn: one RunAgent call on the live
// reminders backend in a staging dir. resumeID threads the two turns of a run so
// the agent remembers, within a run, what it just sent.
func NewAgentTurn(backendFn func() runner.Backend) AgentTurn {
	dir := ""
	if d, err := runner.DefaultAppDir(); err == nil {
		dir = filepath.Join(d, "reminders-staging")
		_ = os.MkdirAll(dir, 0o700)
	}
	return func(prompt, resumeID string) (string, string, error) {
		backend := backendFn()
		if backend == "" || dir == "" {
			return "", "", fmt.Errorf("no reminder agent backend available")
		}
		res, err := runner.RunAgent(backend, dir, prompt, resumeID, runner.RoleRead, false)
		return res.Text, res.SessionID, err
	}
}

// planFirst asks the agent, at registration, WHEN to first reach out — so lead
// time (get-ready, travel) is decided up front and the confirmation can say when.
// Best-effort: on any failure it falls back to "now" (the first run will plan).
func (d *Comp) planFirst(r store.Reminder, now time.Time) (time.Time, string) {
	if d.turn == nil {
		return now, ""
	}
	seed := ""
	if d.seed != nil {
		seed = d.seed(personas.Reminders)
	}
	out, _, err := d.turn(withSeed(seed, firstPlanPrompt(r, now)), "")
	if err != nil {
		d.log.Printf("plan: %v", err)
		return now, ""
	}
	f := parseFields(out)
	note := strings.TrimSpace(f["note"])
	nx := strings.TrimSpace(f["next"])
	if nx == "" || strings.EqualFold(nx, "now") {
		return now, note
	}
	if t, err := timeexpr.Parse(nx, now); err == nil {
		return t, note
	}
	return now, note
}

func firstPlanPrompt(r store.Reminder, now time.Time) string {
	from := strings.TrimSpace(r.FromName)
	if from == "" {
		from = "the owner"
	}
	return strings.Join([]string{
		"A new reminder was just set. Decide WHEN you should first reach out about it.",
		"Task: " + quote(r.Task),
		"Set by: " + from + ". You will be reminding: " + recipientLabel(r) + ".",
		"Right now it is " + now.Format("Mon 2006-01-02 3:04 PM") + " (local time).",
		"",
		"Think about lead time. If this is a 'get ready for / leave for / head to / be at' task tied to a time, reach out WELL BEFORE that time so there is time to actually do it (getting ready often needs 30 to 45 minutes; travel needs the travel time). If they named a time (\"in 20 minutes\", \"at 6\"), honour it, adding lead if it is preparation. If it is open-ended, pick a sensible first moment.",
		"Reply EXACTLY in these two lines and nothing else:",
		"NEXT: when to first reach out. NOW to reach out right away, or a relative time like +45m, +2h, +1d, +2w or +2mo, or an absolute local time like 2026-07-01T17:15.",
		"NOTE: a short note to your future self (the event time, the lead you are giving, what the first message should do).",
	}, "\n")
}

// FireDue is the scheduler-tick entry: it spins up a run for each active reminder
// whose next_at has arrived (skipping any already mid-run).
func (d *Comp) FireDue() {
	if d.turn == nil || !d.cfg().Enabled {
		return
	}
	due, err := d.store.DueReminders(rfc(time.Now()))
	if err != nil {
		d.log.Printf("due scan: %v", err)
		return
	}
	for _, r := range due {
		if d.claimRun(r.ID) {
			go d.runReminder(r)
		}
	}
}

func (d *Comp) claimRun(id int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[id] {
		return false
	}
	d.running[id] = true
	return true
}

func (d *Comp) releaseRun(id int64) {
	d.mu.Lock()
	delete(d.running, id)
	d.mu.Unlock()
}

// runReminder is one full agent run: compose + send (or stay quiet to reschedule),
// wait for the reply, react, then persist the carry-over + next time (or finish)
// and shut off. All the judgement is the agent's.
func (d *Comp) runReminder(r store.Reminder) {
	defer d.releaseRun(r.ID)
	defer func() {
		if rec := recover(); rec != nil {
			d.log.Printf("run #%d panicked: %v", r.ID, rec)
		}
	}()

	cfg := d.cfg()
	// r.Runs counts nudges actually delivered since the recipient last replied,
	// not total wake-ups. The cap is a runaway backstop: it stops a reminder that
	// keeps pestering an unresponsive recipient, while a genuinely recurring one
	// (whose recipient engages, or that only ever quietly reschedules) runs as
	// long as it needs. So quiet planning runs cost nothing and a reply resets the
	// count (both handled below), and the cap no longer kills long-lived reminders.
	if r.Runs >= cfg.MaxRuns {
		r.State, r.NextAt = store.ReminderDone, ""
		d.store.AddReminderEvent(r.ID, "done", "hit the nudge cap without a reply")
		d.save(r)
		return
	}
	d.store.AddReminderEvent(r.ID, "run", "")

	seed := ""
	if d.seed != nil {
		seed = d.seed(personas.Reminders)
	}

	// Turn 1: compose a message (or decide to stay quiet) and a plan.
	t1, sess, err := d.turn(withSeed(seed, planPrompt(r, time.Now())), "")
	if err != nil {
		d.log.Printf("run #%d turn1: %v", r.ID, err)
		r.NextAt = rfc(time.Now().Add(time.Hour)) // don't get stuck; retry later
		d.save(r)
		return
	}
	f1 := parseFields(t1)
	send := strings.TrimSpace(f1["send"])

	if send == "" || strings.EqualFold(send, "none") {
		d.apply(r, f1["next"], f1["note"]) // pure planning run: just (re)schedule, no nudge spent
		return
	}

	r.Runs++ // a nudge is going out: only delivered nudges count toward the cap
	if err := d.sendTo(r.RecipientTransport, r.RecipientAddr, send); err != nil {
		d.log.Printf("run #%d send: %v", r.ID, err)
	}
	d.store.AddReminderEvent(r.ID, "send", send)

	wait := cfg.replyWait()
	if d.replyWaitOverride > 0 {
		wait = d.replyWaitOverride
	}
	rep, got := d.waitReply(r, wait)
	d.store.AddReminderEvent(r.ID, "reply", reportReply(rep, got))
	if got {
		r.Runs = 0 // they engaged, so this isn't a runaway: reset the nudge count
	}

	// Turn 2: react to the reply (or the silence) and decide the outcome.
	t2, _, err := d.turn(withSeed(seed, reactPrompt(r, rep, got, wait)), sess)
	if err != nil {
		d.log.Printf("run #%d turn2: %v", r.ID, err)
		r.NextAt = rfc(time.Now().Add(time.Hour))
		d.save(r)
		return
	}
	f2 := parseFields(t2)
	if out := strings.TrimSpace(f2["reply"]); out != "" && !strings.EqualFold(out, "none") {
		_ = d.sendTo(r.RecipientTransport, r.RecipientAddr, out)
		d.store.AddReminderEvent(r.ID, "send", out)
	}
	d.apply(r, f2["next"], f2["note"])
}

// apply writes the agent's decided outcome: state + next time + carry-over.
func (d *Comp) apply(r store.Reminder, next, note string) {
	r.CarryOver = strings.TrimSpace(note)
	state, at := parseNext(next, time.Now())
	r.State = state
	if state == store.ReminderActive {
		r.NextAt = rfc(at)
	} else {
		r.NextAt = ""
		d.store.AddReminderEvent(r.ID, state, "")
	}
	if r.CarryOver != "" {
		d.store.AddReminderEvent(r.ID, "note", r.CarryOver)
	}
	d.save(r)
}

func (d *Comp) save(r store.Reminder) {
	if err := d.store.UpdateReminder(r); err != nil {
		d.log.Printf("update #%d: %v", r.ID, err)
	}
}

// waitReply registers the recipient and blocks up to wait for their reply.
func (d *Comp) waitReply(r store.Reminder, wait time.Duration) (string, bool) {
	key := recipientKey(r.RecipientTransport, r.RecipientAddr)
	ch := make(chan reply, 1)
	d.mu.Lock()
	d.waiting[key] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.waiting, key)
		d.mu.Unlock()
	}()
	select {
	case rep := <-ch:
		return rep.text, true
	case <-time.After(wait):
		return "", false
	}
}

// --- reply routing (only while a run is actively waiting) --------------------

func (d *Comp) ownsKey(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.waiting[key]
	return ok
}

// Owns reports whether a run is waiting on a Telegram chat's reply.
func (d *Comp) Owns(chatID int64) bool { return d.ownsKey(recipientKey("telegram", itoa(chatID))) }

// OwnsWA reports whether a run is waiting on a WhatsApp jid's reply.
func (d *Comp) OwnsWA(jid string) bool { return d.ownsKey(recipientKey("whatsapp", jid)) }

// FeedTelegram routes a Telegram reply into the waiting run, and marks it read so
// triage doesn't also surface a message the reminder already handled. Returns true
// if consumed.
func (d *Comp) FeedTelegram(chatID, messageID int64, text string) bool {
	if d.feed(recipientKey("telegram", itoa(chatID)), text) {
		if messageID != 0 {
			_ = d.client.MarkRead(chatID, []int64{messageID})
		}
		return true
	}
	return false
}

// FeedWhatsApp routes a WhatsApp reply into the waiting run and clears the unread.
func (d *Comp) FeedWhatsApp(jid, msgRef, text string) bool {
	if d.feed(recipientKey("whatsapp", jid), text) {
		if msgRef != "" {
			_ = d.client.MarkReadOn("whatsapp", jid, []string{msgRef})
		}
		return true
	}
	return false
}

func (d *Comp) feed(key, text string) bool {
	d.mu.Lock()
	ch := d.waiting[key]
	d.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- reply{text: text}:
	default: // a reply is already queued for this run; drop the extra
	}
	return true
}

// --- prompts -----------------------------------------------------------------

func withSeed(seed, body string) string {
	if strings.TrimSpace(seed) == "" {
		return body
	}
	return seed + "\n\n" + body
}

func recipientLabel(r store.Reminder) string {
	if r.RecipientContactID == 0 {
		return "the person who set it (themselves)"
	}
	if r.RecipientName != "" {
		return r.RecipientName
	}
	return "the recipient"
}

func planPrompt(r store.Reminder, now time.Time) string {
	carry := strings.TrimSpace(r.CarryOver)
	if carry == "" {
		carry = "(this is the first run; no notes yet)"
	}
	from := strings.TrimSpace(r.FromName)
	if from == "" {
		from = "the owner"
	}
	return strings.Join([]string{
		fmt.Sprintf("You are handling reminder #%d (%d nudge(s) sent since their last reply).", r.ID, r.Runs),
		"Task to get done: " + quote(r.Task),
		"Set by: " + from + ".",
		"You are reminding: " + recipientLabel(r) + ".",
		"Right now it is " + now.Format("Mon 2006-01-02 3:04 PM") + " (local time).",
		"Your note from last time: " + carry,
		"",
		"Decide what to do right now. You can send a message now, or send nothing yet and just pick a later time to act (for example it is too early and you only want to nudge closer to the moment).",
		"Reply EXACTLY in these three lines and nothing else:",
		"SEND: the message to send now, in a warm, natural, human voice. Never use em-dashes. Or write the single word NONE to stay quiet for now.",
		"NEXT: when to run again. A relative time like +30m, +2h, +1d, +2w or +2mo, or an absolute local time like 2026-07-01T17:30, or DONE if the task is already finished, or CANCEL to stop reminding.",
		"NOTE: a short note to your future self for the next run (what is going on, what you are waiting for, what to do next).",
	}, "\n")
}

func reactPrompt(r store.Reminder, rep string, got bool, wait time.Duration) string {
	heard := "There was no reply within " + humanDur(wait) + "."
	if got {
		heard = recipientLabel(r) + " replied: " + quote(rep)
	}
	return strings.Join([]string{
		fmt.Sprintf("Reminder #%d, task %s.", r.ID, quote(r.Task)),
		heard,
		"",
		"Decide the outcome now. Reply EXACTLY in these three lines and nothing else:",
		"REPLY: an optional short message to send back now, warm and human, never using em-dashes. Or NONE.",
		"NEXT: when to run again (relative like +1h, +1d or +2w, or absolute), or DONE if the task is complete or they made it, or CANCEL to stop reminding.",
		"NOTE: an updated note to your future self for the next run.",
	}, "\n")
}

// --- parsing -----------------------------------------------------------------

var fieldRe = regexp.MustCompile(`(?im)^\s*([A-Za-z]+)\s*:\s*(.+?)\s*$`)

// parseFields pulls "KEY: value" lines into a lowercased-key map (value kept as-is).
func parseFields(text string) map[string]string {
	out := map[string]string{}
	for _, m := range fieldRe.FindAllStringSubmatch(text, -1) {
		k := strings.ToLower(m[1])
		if _, seen := out[k]; !seen {
			out[k] = strings.TrimSpace(m[2])
		}
	}
	return out
}

// parseNext maps the agent's NEXT directive to a state + next run time.
func parseNext(s string, now time.Time) (string, time.Time) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DONE", "FINISHED", "COMPLETE":
		return store.ReminderDone, time.Time{}
	case "CANCEL", "CANCELLED", "STOP":
		return store.ReminderCancelled, time.Time{}
	}
	if t, err := timeexpr.Parse(s, now); err == nil {
		return store.ReminderActive, t
	}
	// Unparseable (or a time in the past, which timeexpr rejects): keep it alive
	// but don't hammer or fire immediately — try again in an hour.
	return store.ReminderActive, now.Add(time.Hour)
}

// --- small helpers -----------------------------------------------------------

func quote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "'") + "\"" }

func reportReply(rep string, got bool) string {
	if !got {
		return "(no reply)"
	}
	return rep
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func humanDur(d time.Duration) string {
	if d < time.Minute {
		return "a moment"
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

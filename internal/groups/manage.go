package groups

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// Admin is the message and group management a call may ask for, on Telegram:
// pin, unpin, edit, a send that never disappears, and the group's
// disappearing timer. The comms client implements it; a Comms without it
// simply cannot manage, and calls asking for it get a note saying so.
type Admin interface {
	Edit(to string, messageID int64, text string) (client.Response, error)
	Pin(to string, messageID int64, o client.PinOptions) (client.Response, error)
	Unpin(to string, messageID int64) (client.Response, error)
	SendKeep(to, text string, o client.KeepOptions) (client.Response, error)
	SetDisappear(to string, timer time.Duration) (client.Response, error)
	SetThreadDisappear(to, thread string, timer time.Duration) (client.Response, error)
	Disappear(to string) (time.Duration, error)
	Delete(to string, messageIDs ...int64) (client.Response, error)
}

// Notes appended to a reply when a management request is not carried out.
const (
	noteManageRole    = "(Only the owner or members with write access can have me pin, unpin, edit or delete my messages.)"
	noteDisappearRole = "(Only the owner can change disappearing messages.)"
	noteTelegramOnly  = "(Pinning, editing and disappearing messages work in Telegram groups only.)"
	noteNoQuoted      = "(Reply to the message you want pinned or unpinned, then ask me.)"
	noteNoEdit        = "(I could not find my earlier message to edit, so here it is again.)"
	noteDeleteRole    = "(Only the owner can have me delete someone's message.)"
	noteNoDelete      = "(I could not find my earlier message to delete.)"
)

// admin returns the comms client's management side, if it has one.
func (h *Handler) admin() (Admin, bool) {
	a, ok := h.Comms.(Admin)
	return a, ok
}

// replyKey names the chat and thread a tracked answer was posted in.
func replyKey(c Call, thread string) string {
	return c.Transport + "|" + c.GroupID + "|" + thread
}

// rememberReply records the id of an answer posted in thread, so a later
// EDIT can find it. Acks and notes are never recorded.
func (h *Handler) rememberReply(c Call, thread string, id int64) {
	if id == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.answers == nil {
		h.answers = map[string]int64{}
	}
	h.answers[replyKey(c, thread)] = id
}

// forgetReply drops a tracked answer that was deleted.
func (h *Handler) forgetReply(c Call, thread string, id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.answers[replyKey(c, thread)] == id {
		delete(h.answers, replyKey(c, thread))
	}
}

func (h *Handler) lastReply(c Call, thread string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.answers[replyKey(c, thread)]
}

// canManage decides in code, before acting, whether the caller may have the
// agent pin, unpin or edit: the owner and write-role members only.
func (h *Handler) canManage(c Call) bool {
	return h.memberRole(c) == store.RoleWrite
}

// manage carries out the call's PIN quoted, UNPIN and DISAPPEAR directives
// (those not tied to posting the answer) and returns notes to append to it.
// The group comes from the call, the message from the call's quote: never
// from model output.
func (h *Handler) manage(c Call, d Directives) []string {
	quoted := d.Pin == "quoted" || d.Unpin
	if !quoted && d.Disappear == "" && d.Delete == "" {
		return nil
	}
	adm, ok := h.admin()
	if !ok || c.Transport != "telegram" {
		return []string{noteTelegramOnly}
	}
	var notes []string
	if quoted {
		switch id := parseID(c.ReplyToID); {
		case !h.canManage(c):
			notes = append(notes, noteManageRole)
		case id == 0:
			notes = append(notes, noteNoQuoted)
		case d.Unpin:
			if _, err := adm.Unpin(c.GroupID, id); err != nil {
				h.logf("group %s/%s: unpin %d: %v", c.Transport, c.GroupID, id, err)
				notes = append(notes, "(I could not unpin it: "+shortErr(err)+".)")
			}
		default:
			if _, err := adm.Pin(c.GroupID, id, client.PinOptions{Silent: true}); err != nil {
				h.logf("group %s/%s: pin %d: %v", c.Transport, c.GroupID, id, err)
				notes = append(notes, "(I could not pin it: "+shortErr(err)+".)")
				break
			}
			if t, err := adm.Disappear(c.GroupID); err == nil && t > 0 {
				notes = append(notes, "(Pinned. If it was sent while disappearing messages were on, it will still disappear: ask me to post it pinned instead.)")
			}
		}
	}
	if d.Delete != "" {
		if n := h.deleteMessage(adm, c, d.Delete); n != "" {
			notes = append(notes, n)
		}
	}
	if d.Disappear != "" {
		notes = append(notes, h.setDisappear(adm, c, d.Disappear, d.DisappearGroup))
	}
	return notes
}

// deleteMessage deletes my last answer in this chat and thread (owner or
// write role), or the quoted message, which may be anyone's (owner only).
// It returns a note when nothing was deleted.
func (h *Handler) deleteMessage(adm Admin, c Call, which string) string {
	var id int64
	switch which {
	case "quoted":
		if !c.SenderIsOwner {
			return noteDeleteRole
		}
		if id = parseID(c.ReplyToID); id == 0 {
			return "(Reply to the message you want deleted, then ask me.)"
		}
	default:
		if !h.canManage(c) {
			return noteManageRole
		}
		if id = h.lastReply(c, c.ThreadID); id == 0 {
			return noteNoDelete
		}
	}
	if _, err := adm.Delete(c.GroupID, id); err != nil {
		h.logf("group %s/%s: delete %d: %v", c.Transport, c.GroupID, id, err)
		return "(I could not delete it: " + shortErr(err) + ".)"
	}
	if which == "last" {
		h.forgetReply(c, c.ThreadID, id)
	}
	return ""
}

// setDisappear changes a timer for the owner and returns the note confirming
// it: the model's reply never decides whether it happened. Asked inside a
// thread it sets that thread's timer (which zcoms enforces, since Telegram
// has one timer per chat), unless wholeGroup; otherwise the group's.
func (h *Handler) setDisappear(adm Admin, c Call, value string, wholeGroup bool) string {
	if !c.SenderIsOwner {
		return noteDisappearRole
	}
	timer, err := parseTimer(value)
	if err != nil {
		return "(I did not change disappearing messages: " + err.Error() + ".)"
	}
	if c.ThreadID != "" && !wholeGroup {
		if _, err := adm.SetThreadDisappear(c.GroupID, c.ThreadID, timer); err != nil {
			h.logf("group %s/%s: thread %s disappear %s: %v", c.Transport, c.GroupID, c.ThreadID, value, err)
			return "(I could not change disappearing messages for this thread: " + shortErr(err) + ".)"
		}
		if timer == 0 {
			return "(Disappearing messages are off in this thread.)"
		}
		return fmt.Sprintf("(New messages in this thread only now disappear after %s. Pinned messages stay.)", timerLabel(timer))
	}
	if timer > 0 && timer%(24*time.Hour) != 0 {
		return "(I did not change disappearing messages: for the whole group Telegram takes whole days, like 1d or 7d.)"
	}
	if _, err := adm.SetDisappear(c.GroupID, timer); err != nil {
		h.logf("group %s/%s: disappear %s: %v", c.Transport, c.GroupID, value, err)
		return "(I could not change disappearing messages: " + shortErr(err) + ".)"
	}
	if timer == 0 {
		return "(Disappearing messages are off in this group.)"
	}
	return fmt.Sprintf("(New messages in this group, in every thread, now disappear after %s. Pinned posts I make stay.)", timerLabel(timer))
}

// post sends an answer to the calling group in thread, quoting replyTo. With
// pin (already allowed by the caller's role) it is sent outside any
// disappearing timer and pinned. The answer's id is remembered for EDIT. It
// returns a note when the pin did not happen.
func (h *Handler) post(c Call, text, thread, replyTo string, pin bool) (string, error) {
	if pin {
		if adm, ok := h.admin(); ok && c.Transport == "telegram" {
			resp, err := adm.SendKeep(c.GroupID, text, client.KeepOptions{Thread: thread, ReplyTo: replyTo, Pin: true, Silent: true})
			if err == nil || resp.MessageID != 0 {
				h.rememberReply(c, thread, resp.MessageID)
				if err != nil {
					h.logf("group %s/%s: pin answer: %v", c.Transport, c.GroupID, err)
					return "", h.note(c, thread, "(I could not pin it: "+shortErr(err)+".)")
				}
				return "", nil
			}
			h.logf("group %s/%s: kept send failed, posting plainly: %v", c.Transport, c.GroupID, err)
		}
	}
	resp, err := h.Comms.SendWith(c.Transport, c.GroupID, text, client.SendOptions{ReplyTo: replyTo, Thread: thread})
	if err != nil {
		return "", err
	}
	h.rememberReply(c, thread, resp.MessageID)
	if pin {
		return noteTelegramOnly, nil
	}
	return "", nil
}

// note posts a short code-written line in thread, untracked.
func (h *Handler) note(c Call, thread, text string) error {
	_, err := h.Comms.SendWith(c.Transport, c.GroupID, text, client.SendOptions{Thread: thread})
	return err
}

// editInstead replaces an earlier answer of mine with text: the message the
// caller quoted if any, else my last answer in this chat and thread. It
// reports whether the edit happened; when not, the caller posts as usual.
func (h *Handler) editInstead(c Call, text string) bool {
	adm, ok := h.admin()
	if !ok || c.Transport != "telegram" {
		return false
	}
	targets := []int64{parseID(c.ReplyToID), h.lastReply(c, c.ThreadID)}
	for _, id := range targets {
		if id == 0 {
			continue
		}
		// Telegram only lets this account edit its own messages, so a
		// quoted message from someone else fails here and the next target
		// is tried.
		if _, err := adm.Edit(c.GroupID, id, text); err != nil {
			h.logf("group %s/%s: edit %d: %v", c.Transport, c.GroupID, id, err)
			continue
		}
		return true
	}
	return false
}

func parseID(s string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// parseTimer reads a DISAPPEAR value: off, or a number of minutes, hours,
// days or weeks (30m, 12h, 7d, 2w). The whole group takes days only.
func parseTimer(v string) (time.Duration, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "off" || v == "0" || v == "never" {
		return 0, nil
	}
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	if n := len(v); n > 1 {
		if u, ok := unit[v[n-1]]; ok {
			k, err := strconv.Atoi(v[:n-1])
			if d := time.Duration(k) * u; err == nil && k > 0 && d <= 366*24*time.Hour {
				return d, nil
			}
		}
	}
	return 0, fmt.Errorf("%q is not a timer like 30m, 12h, 7d, 2w or off", v)
}

func timerLabel(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d < time.Hour:
		return plural(int64(d/time.Minute), "minute")
	case d < day:
		return plural(int64(d/time.Hour), "hour")
	case d%(7*day) == 0 && d/(7*day) == 1:
		return "1 week"
	case d%(7*day) == 0:
		return fmt.Sprintf("%d weeks", d/(7*day))
	case d/day == 1:
		return "1 day"
	}
	return fmt.Sprintf("%d days", d/day)
}

// shortErr is an error's first line, without the tdlib prefix noise.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		s = s[i+2:]
	}
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}

func plural(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

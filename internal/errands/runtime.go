package errands

import (
	"strconv"
	"time"

	"github.com/Zouriel/zcoms/client"
	"github.com/Zouriel/zcoms/whatsapp"
	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// New builds the errands runtime for the unified agent process. The agent owns
// one harness connection and routes events in; errands no longer subscribes on
// its own (that was the standalone component's job).
func New(c *client.Client, waSocket string, waEnabled bool, agents runner.AgentConfig, ownerChat int64) *Comp {
	d := &Comp{
		client:     c,
		waSocket:   waSocket,
		waEnabled:  waEnabled,
		agents:     agents,
		ownerChat:  ownerChat,
		errands:    map[string]*Errand{},
		interviews: map[int64]*interview{},
		scheduled:  map[string]*ScheduledErrand{},
	}
	// Resume persisted errands + their claims, and any scheduled errands.
	if list, err := LoadErrands(); err == nil {
		for _, e := range list {
			d.errands[e.ID] = e
		}
	}
	d.syncClaims()
	if list, err := LoadScheduled(); err == nil {
		for _, s := range list {
			d.scheduled[s.ID] = s
		}
	}
	return d
}

// SetOwnerChat updates where errands report back (resolved once the daemon is up).
func (d *Comp) SetOwnerChat(id int64) { d.ownerChat = id }

// OwnerChat reports the resolved owner chat (0 until resolved).
func (d *Comp) OwnerChat() int64 { return d.ownerChat }

// HandleCommand runs an `errand …` command line and returns the reply (the
// agent.sock entrypoint for `zc errand …`).
func (d *Comp) HandleCommand(text string) string { return d.handleErrandCommand(text) }

// RunInterview conducts a standup interview spec (from the team module) and posts
// the result back. Runs in the background.
func (d *Comp) RunInterview(spec InterviewSpec) { go d.runInterview(spec) }

// FeedTelegram feeds an incoming Telegram reply (from the agent's event router)
// into a matching interview or active errand. Returns true if it was consumed.
func (d *Comp) FeedTelegram(chatID, messageID int64, text, file string) bool {
	if d.feedInterview(chatID, text) {
		return true
	}
	e := d.activeErrandForTG(chatID)
	if e == nil {
		return false
	}
	d.mu.Lock()
	fresh := e.markSeen(strconv.FormatInt(messageID, 10))
	d.mu.Unlock()
	if !fresh || !e.interviewing() {
		return true
	}
	_ = SaveErrand(e)
	d.feedErrand(e, text, file)
	return true
}

// Owns reports whether an active errand/interview claims a Telegram chat, so the
// router knows to send the message here instead of to the bridge.
func (d *Comp) Owns(chatID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.interviews[chatID]; ok {
		return true
	}
	for _, e := range d.errands {
		if e.Source != "wa" && e.TGChat == chatID && e.active() {
			return true
		}
	}
	return false
}

// PollWhatsApp runs one WhatsApp reply-poll pass (driven by the scheduler).
func (d *Comp) PollWhatsApp() { d.pollWAOnce() }

// FireDueScheduled fires any scheduled errands now due (driven by the scheduler).
func (d *Comp) FireDueScheduled() { d.fireDueScheduled(time.Now()) }

// --- helpers that lived in the old standalone main.go -----------------------

func (d *Comp) activeErrandForTG(chatID int64) *Errand {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source != "wa" && e.TGChat == chatID && e.active() {
			return e
		}
	}
	return nil
}

func (d *Comp) activeErrandForWA(jid string) *Errand {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source == "wa" && e.WAChat == jid && e.active() {
			return e
		}
	}
	return nil
}

func (d *Comp) hasActiveWAErrand() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source == "wa" && e.active() {
			return true
		}
	}
	return false
}

// pollWAOnce does a single WhatsApp reply-poll pass for active WA errands.
func (d *Comp) pollWAOnce() {
	if !d.waEnabled || !d.hasActiveWAErrand() {
		return
	}
	unread, err := whatsapp.FetchUnread(d.waSocket)
	if err != nil {
		return
	}
	handled := map[string][]string{}
	for _, u := range unread {
		e := d.activeErrandForWA(u.ChatID)
		if e == nil {
			continue
		}
		d.mu.Lock()
		fresh := e.markSeen(u.MsgID)
		d.mu.Unlock()
		handled[u.ChatID] = append(handled[u.ChatID], u.MsgID)
		if !fresh || !e.interviewing() {
			continue
		}
		_ = SaveErrand(e)
		d.feedErrand(e, u.Text, u.File)
	}
	for jid, ids := range handled {
		_ = whatsapp.Dismiss(d.waSocket, jid, ids)
	}
}

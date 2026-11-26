package errands

import (
	"strconv"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms/client"
)

// New builds the errands runtime for the unified agent process. The agent owns
// one harness connection and routes events in; errands no longer subscribes on
// its own (that was the standalone component's job). WhatsApp goes through the
// daemon's in-process transport — no sidecar.
// agentBrain resolves each errand agent's backend + model from its own persona
// row, read live; nil falls back to whichever CLI is installed.
func New(c *client.Client, waEnabled bool, agentBrain func(persona string) runner.Brain, ownerChat int64, personaSeed func(key string) string) *Comp {
	if agentBrain == nil {
		agentBrain = func(string) runner.Brain { return runner.Brain{Backend: runner.DefaultAgent()} }
	}
	d := &Comp{
		client:      c,
		waEnabled:   waEnabled,
		agentBrain:  agentBrain,
		personaSeed: personaSeed,
		ownerChat:   ownerChat,
		errands:     map[string]*Errand{},
		scheduled:   map[string]*ScheduledErrand{},
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

// FeedTelegram feeds an incoming Telegram reply (from the agent's event router)
// into a matching active errand. Returns true if it was consumed.
func (d *Comp) FeedTelegram(chatID, messageID int64, text, file string) bool {
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

// Owns reports whether an active errand claims a Telegram chat, so the
// router knows to send the message here instead of to the bridge.
func (d *Comp) Owns(chatID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source != "wa" && e.TGChat == chatID && e.active() {
			return true
		}
	}
	return false
}

// OwnsWA reports whether an active errand claims a WhatsApp chat (jid), so the
// agent's event router sends that message to errands instead of the bridge.
func (d *Comp) OwnsWA(jid string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source == "wa" && e.WAChat == jid && e.active() {
			return true
		}
	}
	return false
}

// FeedWhatsApp feeds an incoming WhatsApp reply (from the daemon subscribe
// stream) into a matching active errand and clears it from the daemon's unread
// so triage doesn't also digest a message the errand consumed. Returns true when
// an errand owns the jid (so the router doesn't fall through to the bridge).
func (d *Comp) FeedWhatsApp(jid, msgRef, text, file string) bool {
	e := d.activeErrandForWA(jid)
	if e == nil {
		return false
	}
	d.mu.Lock()
	fresh := e.markSeen(msgRef)
	d.mu.Unlock()
	if msgRef != "" {
		_ = d.client.MarkReadOn("whatsapp", jid, []string{msgRef})
	}
	if !fresh || !e.interviewing() {
		return true
	}
	_ = SaveErrand(e)
	d.feedErrand(e, text, file)
	return true
}

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

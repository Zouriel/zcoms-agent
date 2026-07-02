// Package reschedule negotiates moving an event with the other person on it. The
// owner asks (usually without naming a new time); this fires an agent that reaches
// out to that person on the owner's behalf, talks it through one short message at
// a time in a human voice, agrees a new time, and reports back to the owner (and
// updates the event when a concrete time is settled). The other party's replies
// are treated as conversation only, never as instructions.
package reschedule

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// commsClient is the slice of the comms client the negotiator uses.
type commsClient interface {
	SendOn(transport, to, text string) (client.Response, error)
	MarkRead(chatID int64, messageIDs []int64) error
	MarkReadOn(transport, address string, refs []string) error
	Resolve(to string) (int64, error)
	ResolveContact(name string) ([]client.Contact, error)
}

// AgentTurn runs one model turn for the negotiator: prompt in, text out, with a
// session id threaded so the negotiation keeps its context. nil disables it.
type AgentTurn func(prompt, resumeID string) (text, sessionID string, err error)

// request is one live negotiation.
type request struct {
	reminderID int64
	task       string
	timeLabel  string // the event's current time, human-readable ("" if none)
	note       string // the owner's brief to the negotiator
	targetName string
	transport  string
	addr       string
}

// Comp is the reschedule runtime. Each negotiation runs in its own goroutine,
// parked on the other party's replies via the waiting map.
type Comp struct {
	client commsClient
	store  *store.Store
	turn   AgentTurn
	seed   func(key string) string
	log    *log.Logger

	mu        sync.Mutex
	ownerChat int64
	waiting   map[string]chan reply
	active    map[int64]bool // reminderID -> negotiation in flight

	convWaitOverride time.Duration // tests only
}

type reply struct{ text string }

// New builds the reschedule runtime. turn may be nil (Start then reports the
// feature is unavailable).
func New(c commsClient, st *store.Store, ownerChat int64, seed func(key string) string, turn AgentTurn) *Comp {
	return &Comp{
		client: c, store: st, turn: turn, seed: seed,
		log:       log.New(log.Writer(), "[reschedule] ", log.LstdFlags),
		ownerChat: ownerChat,
		waiting:   map[string]chan reply{},
		active:    map[int64]bool{},
	}
}

// SetOwner updates where the negotiator reports back once the daemon resolves it.
func (d *Comp) SetOwner(ownerChat int64) {
	d.mu.Lock()
	d.ownerChat = ownerChat
	d.mu.Unlock()
}

// Start kicks off a negotiation for event reminderID, guided by note. It returns
// a one-line acknowledgement (or the reason it can't start) for the owner.
func (d *Comp) Start(reminderID int64, note string) string {
	if d.turn == nil {
		return "Rescheduling isn't available right now (no agent backend)."
	}
	r, ok, err := d.store.GetReminder(reminderID)
	if err != nil {
		return "⚠️ couldn't look up that event: " + err.Error()
	}
	if !ok {
		return fmt.Sprintf("No event with id %d.", reminderID)
	}
	if r.State == store.ReminderCancelled {
		return fmt.Sprintf("Event %d (%s) is cancelled, nothing to reschedule.", reminderID, r.Task)
	}
	other := strings.TrimSpace(r.OtherParty)
	if other == "" {
		return fmt.Sprintf("Event %d (%s) has no other party recorded, so there's no one to reach out to. Add one, then try again.", reminderID, r.Task)
	}

	name, transport, addr, msg := d.resolveOther(other)
	if addr == "" {
		return msg // couldn't resolve to a single contact
	}

	d.mu.Lock()
	if d.active[reminderID] {
		d.mu.Unlock()
		return fmt.Sprintf("Already reaching out about event %d (%s).", reminderID, r.Task)
	}
	d.active[reminderID] = true
	d.mu.Unlock()

	req := request{
		reminderID: reminderID,
		task:       r.Task,
		timeLabel:  eventTimeLabel(r),
		note:       strings.TrimSpace(note),
		targetName: name,
		transport:  transport,
		addr:       addr,
	}
	go d.run(req)
	return fmt.Sprintf("📆 Reaching out to %s about rescheduling \"%s\". I'll message you when there's news.", name, r.Task)
}

// resolveOther maps the event's other-party label to a single reachable contact.
// On zero or multiple matches it returns addr="" and an explanatory message.
func (d *Comp) resolveOther(other string) (name, transport, addr, msg string) {
	contacts, err := d.client.ResolveContact(other)
	if err != nil {
		return "", "", "", "⚠️ couldn't look up a contact for \"" + other + "\": " + err.Error()
	}
	switch len(contacts) {
	case 0:
		return "", "", "", fmt.Sprintf("Couldn't find a contact named %q to reach out to. Add them to contacts first, or set the event's other party to match a contact.", other)
	case 1:
		c := contacts[0]
		nm := strings.TrimSpace(c.Name)
		if nm == "" {
			nm = other
		}
		return nm, "telegram", itoa(c.ID), ""
	default:
		var names []string
		for _, c := range contacts {
			label := strings.TrimSpace(c.Name)
			if label == "" {
				label = c.Telegram
			}
			names = append(names, label)
		}
		return "", "", "", fmt.Sprintf("Several contacts match %q: %s. Rename the event's other party to match just one.", other, strings.Join(names, ", "))
	}
}

func (d *Comp) release(reminderID int64) {
	d.mu.Lock()
	delete(d.active, reminderID)
	d.mu.Unlock()
}

func (d *Comp) sendTo(transport, addr, text string) {
	if transport == "" {
		transport = "telegram"
	}
	if _, err := d.client.SendOn(transport, addr, text); err != nil {
		d.log.Printf("send: %v", err)
	}
}

func (d *Comp) reportOwner(text string) {
	d.mu.Lock()
	chat := d.ownerChat
	d.mu.Unlock()
	if chat == 0 {
		d.log.Printf("no owner chat to report to: %s", text)
		return
	}
	if _, err := d.client.SendOn("telegram", itoa(chat), text); err != nil {
		d.log.Printf("report: %v", err)
	}
}

// --- reply routing (only while a negotiation is parked on a reply) -----------

func recipientKey(transport, addr string) string {
	if transport == "" {
		transport = "telegram"
	}
	return transport + "|" + addr
}

func (d *Comp) ownsKey(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.waiting[key]
	return ok
}

// Owns reports whether a negotiation is waiting on a Telegram chat's reply.
func (d *Comp) Owns(chatID int64) bool { return d.ownsKey(recipientKey("telegram", itoa(chatID))) }

// OwnsWA reports whether a negotiation is waiting on a WhatsApp jid's reply.
func (d *Comp) OwnsWA(jid string) bool { return d.ownsKey(recipientKey("whatsapp", jid)) }

// FeedTelegram routes a Telegram reply into the waiting negotiation and marks it
// read. Returns true if consumed.
func (d *Comp) FeedTelegram(chatID, messageID int64, text string) bool {
	if d.feed(recipientKey("telegram", itoa(chatID)), text) {
		if messageID != 0 {
			_ = d.client.MarkRead(chatID, []int64{messageID})
		}
		return true
	}
	return false
}

// FeedWhatsApp routes a WhatsApp reply into the waiting negotiation.
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
	default:
	}
	return true
}

func (d *Comp) waitReply(transport, addr string, wait time.Duration) (string, bool) {
	key := recipientKey(transport, addr)
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// LiveBackend resolves the reschedule task's backend from the persona rows live.
func LiveBackend(st *store.Store) runner.Backend {
	cfg := runner.AgentConfig{Tasks: map[string]runner.Backend{}}
	if ps, err := st.ListPersonas(); err == nil {
		for _, p := range ps {
			cfg.Tasks[p.Key] = runner.Backend(p.Backend)
		}
	}
	return cfg.For("reschedule", "")
}

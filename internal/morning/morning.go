// Package morning is the daily morning agent. Once a day (by default around 6am,
// gated by a live enable/disable setting) it greets the owner, waits until they
// actually reply, then walks them through the events they have for that day in a
// warm human voice and offers to reschedule or add anything. It reads and edits
// the owner's own reminders (the system's event store) through a controlled,
// deterministic path: the model only decides what to say and which structured
// action to take, the store writes stay in code. All the judgement lives in the
// agent; the loop just carries it.
package morning

import (
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// commsClient is the slice of the comms client the morning agent uses.
type commsClient interface {
	SendOn(transport, to, text string) (client.Response, error)
	MarkRead(chatID int64, messageIDs []int64) error
	MarkReadOn(transport, address string, refs []string) error
	Resolve(to string) (int64, error)
}

// AgentTurn runs one model turn for the morning agent: prompt in, text out, with
// a session id threaded so the turns of a briefing share context. A fake stands
// in for unit tests; nil disables the agent (the session is skipped).
type AgentTurn func(prompt, resumeID string) (text, sessionID string, err error)

// Comp is the morning-agent runtime. One briefing runs per day, in its own
// goroutine, parked on the owner's replies via the waiting map.
type Comp struct {
	client commsClient
	store  *store.Store
	turn   AgentTurn
	seed   func(key string) string
	log    *log.Logger

	mu        sync.Mutex
	mainUser  string
	ownerChat int64
	waiting   map[string]chan reply // recipient key -> the parked wait
	active    bool                  // a briefing is in flight (at most one/day)

	// test knobs (0 = use the real defaults).
	wakeWaitOverride time.Duration
	convWaitOverride time.Duration
}

type reply struct{ text string }

// New builds the morning runtime. turn may be nil (then Fire is a no-op with a
// log line, e.g. in a headless test without a backend).
func New(c commsClient, st *store.Store, mainUser string, ownerChat int64, seed func(key string) string, turn AgentTurn) *Comp {
	return &Comp{
		client: c, store: st, turn: turn, seed: seed,
		log:       log.New(log.Writer(), "[morning] ", log.LstdFlags),
		mainUser:  mainUser,
		ownerChat: ownerChat,
		waiting:   map[string]chan reply{},
	}
}

// SetOwner updates the owner identity once the daemon resolves it.
func (d *Comp) SetOwner(mainUser string, ownerChat int64) {
	d.mu.Lock()
	d.mainUser, d.ownerChat = mainUser, ownerChat
	d.mu.Unlock()
}

// Fire is the scheduler's daily hook. It runs a briefing when the feature is
// enabled and one is not already in flight; the actual session runs in its own
// goroutine so it never blocks the scheduler tick while waiting on a reply.
func (d *Comp) Fire() {
	if d.turn == nil {
		return
	}
	if !LoadConfig(d.store).Enabled {
		return
	}
	d.mu.Lock()
	if d.active {
		d.mu.Unlock()
		return
	}
	d.active = true
	d.mu.Unlock()
	go d.runSession()
}

func (d *Comp) releaseSession() {
	d.mu.Lock()
	d.active = false
	d.mu.Unlock()
}

// ownerRoute is the transport + reply address for the owner's own chat.
func (d *Comp) ownerRoute() (transport, addr string) {
	d.mu.Lock()
	chat, user := d.ownerChat, d.mainUser
	d.mu.Unlock()
	if chat == 0 && user != "" {
		if id, err := d.client.Resolve(user); err == nil {
			chat = id
			d.mu.Lock()
			d.ownerChat = id
			d.mu.Unlock()
		}
	}
	if chat == 0 {
		return "", ""
	}
	return "telegram", itoa(chat)
}

func (d *Comp) sendTo(transport, addr, text string) {
	if transport == "" {
		transport = "telegram"
	}
	if _, err := d.client.SendOn(transport, addr, text); err != nil {
		d.log.Printf("send: %v", err)
	}
}

// --- reply routing (only while a briefing is parked on a reply) --------------

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

// Owns reports whether the briefing is waiting on a Telegram chat's reply.
func (d *Comp) Owns(chatID int64) bool { return d.ownsKey(recipientKey("telegram", itoa(chatID))) }

// OwnsWA reports whether the briefing is waiting on a WhatsApp jid's reply.
func (d *Comp) OwnsWA(jid string) bool { return d.ownsKey(recipientKey("whatsapp", jid)) }

// FeedTelegram routes a Telegram reply into the waiting briefing and marks it
// read so triage doesn't also surface it. Returns true if consumed.
func (d *Comp) FeedTelegram(chatID, messageID int64, text string) bool {
	if d.feed(recipientKey("telegram", itoa(chatID)), text) {
		if messageID != 0 {
			_ = d.client.MarkRead(chatID, []int64{messageID})
		}
		return true
	}
	return false
}

// FeedWhatsApp routes a WhatsApp reply into the waiting briefing and clears the unread.
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
	default: // a reply is already queued for this turn; drop the extra
	}
	return true
}

// waitReply parks on the owner's reply up to wait, then gives up.
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

// LiveBackend resolves the morning task's agent backend from the persona rows
// live, so a console backend change applies with no restart.
func LiveBackend(st *store.Store) runner.Backend {
	cfg := runner.AgentConfig{Tasks: map[string]runner.Backend{}}
	if ps, err := st.ListPersonas(); err == nil {
		for _, p := range ps {
			cfg.Tasks[p.Key] = runner.Backend(p.Backend)
		}
	}
	return cfg.For("morning", "")
}

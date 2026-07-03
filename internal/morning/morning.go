// Package morning is the daily morning agent. Once a day (by default around 6am,
// gated by a live enable/disable setting) it greets the owner and then goes
// dormant. Unlike a parked briefing, it holds no goroutine and no timeout: like
// the bridge, the session stays open across gaps and every message the owner
// sends drives one more turn, until they say "end". It reads and edits the
// owner's own reminders (the system's event store) through a controlled,
// deterministic path: the model only decides what to say and which structured
// action to take, the store writes stay in code. All the judgement lives in the
// agent; the loop just carries it.
package morning

import (
	"log"
	"strconv"
	"sync"

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

// Comp is the morning-agent runtime. A session opens once a day (Fire) and then
// lives as a plain message-driven conversation: no goroutine is parked on a
// reply, and nothing times it out. The owner's next message always wakes it
// again, until they end it.
type Comp struct {
	client commsClient
	store  *store.Store
	turn   AgentTurn
	seed   func(key string) string
	log    *log.Logger

	mu        sync.Mutex
	mainUser  string
	ownerChat int64

	active    bool   // a morning session is open (until the owner says end)
	first     bool   // the next owner reply is the wake/brief turn
	busy      bool   // a turn (greeting or reply) is in flight
	sessionID string // resume id threading this morning's turns
}

// New builds the morning runtime. turn may be nil (then Fire is a no-op with a
// log line, e.g. in a headless test without a backend).
func New(c commsClient, st *store.Store, mainUser string, ownerChat int64, seed func(key string) string, turn AgentTurn) *Comp {
	return &Comp{
		client: c, store: st, turn: turn, seed: seed,
		log:       log.New(log.Writer(), "[morning] ", log.LstdFlags),
		mainUser:  mainUser,
		ownerChat: ownerChat,
	}
}

// SetOwner updates the owner identity once the daemon resolves it.
func (d *Comp) SetOwner(mainUser string, ownerChat int64) {
	d.mu.Lock()
	d.mainUser, d.ownerChat = mainUser, ownerChat
	d.mu.Unlock()
}

// Fire is the scheduler's daily hook. It opens a morning session and sends the
// greeting, then returns: the session goes dormant and the owner's own replies
// drive it from there (no parked goroutine, no timeout). At most one session is
// open at a time.
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
	// busy is held across the greeting so an eager reply waits for it to land.
	d.active, d.first, d.busy, d.sessionID = true, true, true, ""
	d.mu.Unlock()
	go d.sendGreeting()
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

// --- reply routing (for the life of an open session) -------------------------

// Owns reports whether an open morning session should take this Telegram chat's
// message. Unlike a parked briefing, it stays true for the whole session (the
// owner's own chat) until the session ends, so every reply keeps waking it.
func (d *Comp) Owns(chatID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active && d.ownerChat != 0 && chatID == d.ownerChat
}

// OwnsWA is always false: the morning briefing runs on the owner's Telegram chat.
func (d *Comp) OwnsWA(string) bool { return false }

// FeedTelegram routes an owner reply into the open morning session and marks it
// read so triage doesn't also surface it. Returns true if consumed.
func (d *Comp) FeedTelegram(chatID, messageID int64, text string) bool {
	if !d.Owns(chatID) {
		return false
	}
	if messageID != 0 {
		_ = d.client.MarkRead(chatID, []int64{messageID})
	}
	d.handleReply(text)
	return true
}

// FeedWhatsApp is a no-op: the morning session lives on the owner's Telegram chat.
func (d *Comp) FeedWhatsApp(string, string, string) bool { return false }

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

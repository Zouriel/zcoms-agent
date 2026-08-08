// Package lineup is the owner's on-demand schedule assistant. The owner opens it
// by saying "lineup" (optionally followed by a request) on their own chat; from
// then on every message they send drives one more turn, until they say "end" or
// "stop", or they go quiet for a few minutes. It is deliberately narrow: it can
// read the owner's agenda, add an event, or reschedule (move) one, and nothing
// else, no shell, files, or messaging. Like the morning agent it holds no parked
// goroutine; the only timer is the idle close. Schedule writes stay in code, the
// model only chooses what to say and which structured action to take.
package lineup

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// defaultIdleWait is how long the lineup stays open with no owner message before
// it closes itself.
const defaultIdleWait = 5 * time.Minute

// commsClient is the slice of the comms client the lineup uses.
type commsClient interface {
	SendOn(transport, to, text string) (client.Response, error)
	MarkRead(chatID int64, messageIDs []int64) error
	Resolve(to string) (int64, error)
}

// AgentTurn runs one model turn: prompt in, text out, with a session id threaded
// so the turns of a session share context. nil disables the agent (tests/headless).
type AgentTurn func(prompt, resumeID string) (text, sessionID string, err error)

// Comp is the lineup runtime. A session opens on the "lineup" keyword and lives
// as a plain message-driven conversation guarded by an idle timer.
type Comp struct {
	client   commsClient
	store    *store.Store
	turn     AgentTurn
	seed     func(key string) string
	log      *log.Logger
	idleWait time.Duration

	// startResch hands a shared event to the reschedule negotiator (which texts the
	// other party on the owner's behalf) and returns a one-line acknowledgement. nil
	// when the negotiator isn't wired (tests/headless).
	startResch func(eventID int64, note string) string

	// remindOther sets a reminder for someone who is NOT the owner and returns the
	// reminders module's acknowledgement. The schedule agents only add to the
	// owner's own calendar, so a "remind <someone else> to …" request has to leave
	// here or it becomes an owner event addressed to the wrong person. nil when
	// unwired (tests/headless), in which case the request stays on the calendar.
	remindOther func(who, task string) string

	mu        sync.Mutex
	mainUser  string
	ownerChat int64

	active    bool        // a lineup session is open
	first     bool        // the next turn is the opening one
	busy      bool        // a turn is in flight
	sessionID string      // resume id threading this session's turns
	idle      *time.Timer // fires to close an idle session
}

// New builds the lineup runtime. turn may be nil (then Trigger is inert).
// startResch may be nil (the negotiate action then reports it's unavailable).
func New(c commsClient, st *store.Store, mainUser string, ownerChat int64, seed func(key string) string, turn AgentTurn, startResch func(eventID int64, note string) string) *Comp {
	return &Comp{
		client: c, store: st, turn: turn, seed: seed,
		startResch: startResch,
		log:        log.New(log.Writer(), "[lineup] ", log.LstdFlags),
		idleWait:   defaultIdleWait,
		mainUser:   mainUser,
		ownerChat:  ownerChat,
	}
}

// SetRemindOther wires the delegate that sets a reminder for a third party (the
// reminders module, which applies its own trust gate). Safe to leave unset.
func (d *Comp) SetRemindOther(fn func(who, task string) string) {
	d.mu.Lock()
	d.remindOther = fn
	d.mu.Unlock()
}

// SetOwner updates the owner identity once the daemon resolves it.
func (d *Comp) SetOwner(mainUser string, ownerChat int64) {
	d.mu.Lock()
	d.mainUser, d.ownerChat = mainUser, ownerChat
	d.mu.Unlock()
}

// Trigger routes one owner message. It returns true when the lineup consumed it:
// either a session is open (any message continues it), or the "lineup" keyword
// opens one. A non-owner message, or an ordinary owner message with no open
// session, is left for the bridge.
func (d *Comp) Trigger(chatID, messageID int64, text string) bool {
	if d.turn == nil {
		return false
	}
	owner := d.ownerChatID()
	if owner == 0 || chatID != owner {
		return false
	}
	kw, rest := splitKeyword(text)
	d.mu.Lock()
	active := d.active
	d.mu.Unlock()
	if !kw && !active {
		return false // an ordinary owner message; the bridge handles it
	}
	if messageID != 0 {
		_ = d.client.MarkRead(chatID, []int64{messageID})
	}
	msg := strings.TrimSpace(text)
	if kw {
		msg = rest
	}
	d.handleMessage(msg, kw)
	return true
}

// splitKeyword reports whether text opens the lineup ("lineup" as the first word)
// and returns the rest of the message. "lineups" and other words do not match.
func splitKeyword(text string) (bool, string) {
	t := strings.TrimSpace(text)
	if len(t) < 6 || !strings.EqualFold(t[:6], "lineup") {
		return false, ""
	}
	rest := t[6:]
	if rest == "" {
		return true, ""
	}
	switch rest[0] {
	case ' ', ':', ',', '-', '\n', '\t':
		return true, strings.TrimSpace(strings.TrimLeft(rest, " :,-\n\t"))
	}
	return false, ""
}

// ownerChatID returns the owner's Telegram chat id, resolving and caching it from
// main_user the first time when it was not known at startup.
func (d *Comp) ownerChatID() int64 {
	d.mu.Lock()
	chat, user := d.ownerChat, d.mainUser
	d.mu.Unlock()
	if chat != 0 || user == "" {
		return chat
	}
	id, err := d.client.Resolve(user)
	if err != nil {
		return 0
	}
	d.mu.Lock()
	d.ownerChat = id
	d.mu.Unlock()
	return id
}

// ownerRoute is the transport + reply address for the owner's own chat.
func (d *Comp) ownerRoute() (transport, addr string) {
	if id := d.ownerChatID(); id != 0 {
		return "telegram", itoa(id)
	}
	return "", ""
}

func (d *Comp) sendTo(transport, addr, text string) {
	if transport == "" {
		transport = "telegram"
	}
	if _, err := d.client.SendOn(transport, addr, text); err != nil {
		d.log.Printf("send: %v", err)
	}
}

func (d *Comp) seedOr() string {
	if d.seed == nil {
		return ""
	}
	return d.seed("lineup")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// LiveBrain resolves the lineup's backend + model from its persona row live, so
// a console change applies with no restart.
func LiveBrain(st *store.Store) runner.Brain {
	return personas.Brain(st, personas.Lineup)
}

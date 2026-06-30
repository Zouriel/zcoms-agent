// Package reminders is the agent-driven reminder loop. A reminder is not a state
// machine but a small instance a dedicated "reminder agent" advances one run at a
// time: at the scheduled time it wakes, composes + sends a message in a warm human
// voice, waits for the reply, then writes a carry-over note + the next time (or
// finishes) and shuts off. All the judgement — tone, when to nudge, whether it's
// done, whether someone's mid-event — lives in the agent, not in hardcoded logic.
// The carry-over is the whole memory, so it stays coherent over any number of runs.
package reminders

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// commsClient is the slice of the comms client the reminders runtime uses.
type commsClient interface {
	SendOn(transport, to, text string) (client.Response, error)
	MarkRead(chatID int64, messageIDs []int64) error            // Telegram
	MarkReadOn(transport, address string, refs []string) error // other transports
	Resolve(to string) (int64, error)
	ResolveContact(name string) ([]client.Contact, error)
}

// AgentTurn runs one model turn for the reminder agent: prompt in, text out, with
// a session id threaded so the two turns of a single run share context. A fake
// stands in for unit tests.
type AgentTurn func(prompt, resumeID string) (text, sessionID string, err error)

// Comp is the reminders runtime. agent.db is the source of truth; the scheduler
// tick scans it for due rows and spins up a run per reminder.
type Comp struct {
	client commsClient
	store  *store.Store
	turn   AgentTurn
	seed   func(key string) string
	log    *log.Logger

	mu        sync.Mutex
	mainUser  string                // owner handle (the §6 owner check)
	ownerChat int64                 // resolved owner Telegram chat id (for "me" on agent.sock)
	waiting   map[string]chan reply // recipient key ("transport|addr") -> the run waiting on its reply
	running   map[int64]bool        // reminders with a run in flight (so the tick won't double-fire)

	replyWaitOverride time.Duration // tests only: shorten the reply wait (0 = use config)
}

// reply is an inbound message routed into a waiting run.
type reply struct{ text string }

// New builds the reminders runtime. turn may be nil (then runs are skipped with a
// log line — e.g. no backend in a test harness that doesn't exercise running).
func New(c commsClient, st *store.Store, mainUser string, ownerChat int64, seed func(key string) string, turn AgentTurn) *Comp {
	return &Comp{
		client: c, store: st, turn: turn, seed: seed,
		log:       log.New(log.Writer(), "[reminders] ", log.LstdFlags),
		mainUser:  mainUser,
		ownerChat: ownerChat,
		waiting:   map[string]chan reply{},
		running:   map[int64]bool{},
	}
}

// SetOwner updates the owner identity once the daemon resolves it.
func (d *Comp) SetOwner(mainUser string, ownerChat int64) {
	d.mu.Lock()
	d.mainUser, d.ownerChat = mainUser, ownerChat
	d.mu.Unlock()
}

func (d *Comp) owner() (string, int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mainUser, d.ownerChat
}

// sendTo delivers text to a recipient on its transport.
func (d *Comp) sendTo(transport, addr, text string) error {
	if transport == "" {
		transport = "telegram"
	}
	_, err := d.client.SendOn(transport, addr, text)
	return err
}

// recipientKey is the wait-map / routing key for a reminder's recipient.
func recipientKey(transport, addr string) string {
	if transport == "" {
		transport = "telegram"
	}
	return transport + "|" + addr
}

// allowSet builds the live set of allow-list keys for the §6 trust gate.
func (d *Comp) allowSet() map[string]bool {
	set := map[string]bool{}
	es, err := d.store.ListAllow()
	if err != nil {
		d.log.Printf("allow set: %v", err)
		return set
	}
	for _, e := range es {
		set[runner.AllowKey(e.Platform, e.Handle)] = true
	}
	return set
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

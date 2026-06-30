// Package reminders is the agent's stateful reminder loop: a reminder is not a
// timer but a persisted object the agent advances at every scheduler tick, ending
// only when the *task* is confirmed done (or a deadline window passes). It lives
// in the agent tier because it needs in-process access to the runner (to classify
// cadence and replies), the scheduler (to wait), the comms client (to reach the
// reminded party), and the inbound stream (to catch their replies). See
// zcoms-FEATURE-reminders.md.
package reminders

import (
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// commsClient is the slice of the comms client the reminders runtime uses: send
// on a transport, clear a WhatsApp unread, and resolve a name/handle. *client.Client
// satisfies it; a fake satisfies it in tests.
type commsClient interface {
	SendOn(transport, to, text string) (client.Response, error)
	MarkReadOn(transport, address string, refs []string) error
	Resolve(to string) (int64, error)
	ResolveContact(name string) ([]client.Contact, error)
}

// Comp is the reminders runtime. The store (agent.db) is the source of truth — the
// scheduler tick and the reply matcher both read it fresh — so the loop survives a
// restart with no in-memory state to rebuild.
type Comp struct {
	client   commsClient
	store    *store.Store
	classify Classifier
	composer Composer                // writes the humane message lines (may be nil → templates)
	seed     func(key string) string // owner-editable persona scaffold (agent.db)
	log      *log.Logger

	mu        sync.Mutex
	mainUser  string // owner handle (settings.MainUser) — the owner trust check
	ownerChat int64  // resolved owner Telegram chat id (for "me" on the agent.sock path)
}

// New builds the reminders runtime. mainUser/ownerChat are refreshed by the agent
// once the daemon resolves them. classify may be nil — a heuristic is used then.
func New(c *client.Client, st *store.Store, mainUser string, ownerChat int64, seed func(key string) string, classify Classifier, composer Composer) *Comp {
	if classify == nil {
		classify = heuristic{}
	}
	return &Comp{
		client: c, store: st, classify: classify, composer: composer, seed: seed,
		log:       log.New(log.Writer(), "[reminders] ", log.LstdFlags),
		mainUser:  mainUser,
		ownerChat: ownerChat,
	}
}

// SetOwner updates the owner identity once the daemon resolves it (mirrors
// errands.SetOwnerChat).
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

// --- sending -----------------------------------------------------------------

// sendTo delivers text to a reminder's target on its transport. Telegram targets
// are addressed by chat-id string, WhatsApp by jid — both via the comms client.
func (d *Comp) sendTo(transport, addr, text string) error {
	if transport == "" {
		transport = "telegram"
	}
	_, err := d.client.SendOn(transport, addr, text)
	return err
}

// --- allowlist membership (for the §6 trust gate) ----------------------------

// allowSet builds the live set of allow-list keys ("transport|handle") from
// agent.db, so the trust check sees adds/removes without a restart.
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

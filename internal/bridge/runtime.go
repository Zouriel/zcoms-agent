package bridge

import (
	"strconv"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/reminders"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms/client"
)

// Deps are the bridge runtime's dependencies, built by the agent process from
// agent.db (allowlist, workspaces→locations, persona backends, settings).
type Deps struct {
	Client     *client.Client
	WAEnabled  bool
	Locations  runner.Locations
	Allow      runner.Allowlist
	Settings   runner.Settings
	MainChatID int64
	// AgentBrain resolves a persona's backend + model from agent.db, read live per
	// turn so a console edit applies with no restart. override pins one CLI (an
	// allow-listed user's agent). May be nil, then every session runs on whichever
	// CLI is installed, claude first.
	AgentBrain func(persona string, override runner.Backend) runner.Brain
	// PersonaSeed returns a persona's owner-editable seed prompt (from agent.db),
	// read live per call so console edits take effect without a restart. May be nil.
	PersonaSeed func(key string) string
	// Reminders is the in-process reminder loop the `remind …` command drives. May
	// be nil (then the command replies that reminders are unavailable).
	Reminders *reminders.Comp
	// ErrandCmd runs an `errand …` command against the in-process errands runtime
	// and returns its reply. May be nil (then errand commands report unavailable).
	ErrandCmd func(text string) string
	// Phrase returns an editable canned message by key (override from agent.db,
	// else the default), read live. May be nil (then the compiled default is used).
	Phrase func(key string) string
	// ProjectNote returns extra first-turn guidance for a workspace folder
	// bound to a project group ("" when it is not bound), so progress
	// updates the owner asks for go to the group via `zc project say`.
	ProjectNote func(path string) string
}

// New builds the interactive bridge runtime for the unified agent process.
func New(d Deps) *Comp {
	brain := d.AgentBrain
	if brain == nil {
		// No resolver wired (tests / headless): fall back to any installed CLI.
		brain = func(string, runner.Backend) runner.Brain {
			return runner.Brain{Backend: runner.DefaultAgent()}
		}
	}
	return &Comp{
		client:      d.Client,
		waEnabled:   d.WAEnabled,
		locations:   d.Locations,
		allow:       d.Allow,
		settings:    d.Settings,
		mainChatID:  d.MainChatID,
		personaSeed: d.PersonaSeed,
		agentBrain:  brain,
		reminders:   d.Reminders,
		errandCmd:   d.ErrandCmd,
		phrase:      d.Phrase,
		projectNote: d.ProjectNote,
		byUser:      map[string]*userState{},
	}
}

// brain resolves the backend + model for a session's persona, defaulting to the
// bridge persona when the session has not picked one (plain chat / no location).
func (d *Comp) brain(persona string, override Backend) Brain {
	if strings.TrimSpace(persona) == "" {
		persona = personas.Bridge
	}
	return d.agentBrain(persona, override)
}

// HandleEvent dispatches one allow-listed user's incoming message (the agent's
// event router calls this for messages not claimed by an errand). Returns false
// if the sender isn't allow-listed, so the router can fall through.
func (d *Comp) HandleEvent(ev client.Event) bool {
	// A group call is never a 1:1 conversation: the sender is a group member,
	// not an allow-listed principal, and the reply target is the group. The
	// groups handler owns these; the bridge must not open a session for one.
	if ev.Group {
		return false
	}
	st := d.stateFor(ev)
	if st == nil {
		// Not allow-listed: no immediate reply. The message is stored as unread;
		// triage decides which non-allow-listed messages are important and sends
		// the canned auto-reply only to those (avoids spamming everyone).
		return false
	}
	if ev.Kind != "" && ev.Kind != "messageText" {
		d.handleIncomingFile(st, ev.File, "", ev.Text)
		return true
	}
	d.handle(st, strings.TrimSpace(ev.Text))
	return true
}

// stateFor returns (creating on first contact) the per-user session state for an
// allow-listed sender on whatever transport the event arrived on, or nil if the
// sender isn't allow-listed. The reply address is refreshed each call so a reply
// always returns on the same app — and transport — the message came in on.
func (d *Comp) stateFor(ev client.Event) *userState {
	tp := ev.Transport
	if tp == "" {
		tp = "telegram"
	}

	// Per transport: the allow-match handle, the stable session id, and the
	// native reply address.
	var matchHandle, nativeID, address string
	switch tp {
	case "whatsapp":
		digits := WADigits(ev.Address)
		matchHandle, nativeID, address = digits, digits, ev.Address
	default: // telegram
		matchHandle = ev.Sender
		nativeID = strconv.FormatInt(ev.UserID, 10)
		address = ev.Address
		if address == "" {
			address = strconv.FormatInt(ev.ChatID, 10)
		}
	}
	if nativeID == "" || nativeID == "0" {
		return nil
	}

	key := sessionKey(tp, nativeID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.byUser[key]; ok {
		st.address = address // refresh reply target
		return st
	}
	handle, entry, ok := d.lookupAllow(tp, matchHandle)
	if !ok {
		return nil
	}
	st := &userState{
		username:  handle, // the allow-list handle (canonical), not the raw event value
		entry:     entry,
		transport: tp,
		address:   address,
		// persona stays "" until a location/chat/triage session is picked: the
		// bridge persona governs an unscoped session, resolved live per turn.
	}
	d.byUser[key] = st
	return st
}

// lookupAllow finds an allow-list entry for an inbound (platform, sender). Both
// sides go through AllowKey, so Telegram @usernames match case-insensitively and
// WhatsApp numbers match regardless of +/spaces/jid-suffix. Returns the matched
// handle for downstream actor/identity use.
func (d *Comp) lookupAllow(platform, sender string) (string, AllowEntry, bool) {
	key := AllowKey(platform, sender)
	e, ok := d.allow[key]
	if !ok {
		return "", AllowEntry{}, false
	}
	handle := key
	if i := strings.IndexByte(key, '|'); i >= 0 {
		handle = key[i+1:]
	}
	return handle, e, true
}

// SetSettings swaps in freshly-built settings (after an owner change) so toggles
// like auto-reply take effect live with no restart. d.settings is read under
// d.mu (maybeAutoReply / currentTriage), so the swap is race-free.
func (d *Comp) SetSettings(s Settings) {
	d.mu.Lock()
	d.settings = s
	d.mu.Unlock()
}

// SetAllow swaps in a freshly-built allowlist (after an owner add/remove) so the
// change takes effect live with no restart. It also evicts any active session
// whose principal is no longer allow-listed — otherwise an existing session
// would keep being served, since stateFor reuses a session without re-checking
// the allowlist. d.allow is only ever read inside stateFor under d.mu, so the
// swap is race-free.
func (d *Comp) SetAllow(allow Allowlist) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allow = allow
	for key, st := range d.byUser {
		if _, ok := allow[AllowKey(st.transport, st.username)]; !ok {
			delete(d.byUser, key)
		}
	}
}

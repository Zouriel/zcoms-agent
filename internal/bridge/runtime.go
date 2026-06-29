package bridge

import (
	"strconv"
	"strings"

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
	Agents     runner.AgentConfig
	Settings   runner.Settings
	MainChatID int64
	// PersonaSeed returns a persona's owner-editable seed prompt (from agent.db),
	// read live per call so console edits take effect without a restart. May be nil.
	PersonaSeed func(key string) string
}

// New builds the interactive bridge runtime for the unified agent process.
func New(d Deps) *Comp {
	return &Comp{
		client:           d.Client,
		waEnabled:        d.WAEnabled,
		bridgeBackend:    d.Agents.For("bridge", ""),
		chatBackend:      d.Agents.For("chat", ""),
		triageBackend:    d.Agents.For("triage", ""),
		workspaceBackend: d.Agents.For("workspace", ""),
		locations:        d.Locations,
		allow:            d.Allow,
		agents:           d.Agents,
		settings:         d.Settings,
		mainChatID:       d.MainChatID,
		personaSeed:      d.PersonaSeed,
		byUser:           map[string]*userState{},
	}
}

// HandleEvent dispatches one allow-listed user's incoming message (the agent's
// event router calls this for messages not claimed by an errand). Returns false
// if the sender isn't allow-listed, so the router can fall through.
func (d *Comp) HandleEvent(ev client.Event) bool {
	st := d.stateFor(ev)
	if st == nil {
		return false // not allow-listed
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
	default: // telegram (Instagram joins here later)
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
		backend:   d.agents.For("bridge", entry.Agent),
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

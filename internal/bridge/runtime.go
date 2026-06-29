package bridge

import (
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms/client"
)

// Deps are the bridge runtime's dependencies, built by the agent process from
// agent.db (allowlist, workspaces→locations, persona backends, settings).
type Deps struct {
	Client     *client.Client
	WASocket   string
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
		waSocket:         d.WASocket,
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
		byUser:           map[int64]*userState{},
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
// allow-listed sender, or nil if the sender isn't allow-listed.
func (d *Comp) stateFor(ev client.Event) *userState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.byUser[ev.UserID]; ok {
		st.chatID = ev.ChatID
		return st
	}
	handle, entry, ok := d.lookupAllow(ev.Sender)
	if !ok {
		return nil
	}
	st := &userState{
		username: handle, // the allow-list handle (canonical case), not the raw event value
		entry:    entry,
		chatID:   ev.ChatID,
		backend:  d.agents.For("bridge", entry.Agent),
	}
	d.byUser[ev.UserID] = st
	return st
}

// lookupAllow finds an allow-list entry for a sender handle, case-insensitively
// (Telegram @usernames are case-insensitive, so a casing difference between the
// authored allow-list and TDLib's canonical form must not drop the message).
// Returns the stored handle so downstream actor/identity use is consistent.
func (d *Comp) lookupAllow(sender string) (string, AllowEntry, bool) {
	if e, ok := d.allow[sender]; ok {
		return sender, e, true
	}
	want := strings.ToLower(strings.TrimSpace(sender))
	for h, e := range d.allow {
		if strings.ToLower(strings.TrimSpace(h)) == want {
			return h, e, true
		}
	}
	return "", AllowEntry{}, false
}

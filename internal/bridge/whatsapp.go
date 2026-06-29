package bridge

import (
	"strconv"
	"strings"
)

// HandleWhatsApp routes one inbound WhatsApp message (fed by the agent's WA poll)
// into the bridge, exactly like an allow-listed Telegram message. jid is the
// sender's chat jid (used for replies); returns true when the sender is
// allow-listed for WhatsApp (and was therefore consumed) so the poll can mark it
// read. WhatsApp has no real-time push, so this path is poll-driven.
func (d *Comp) HandleWhatsApp(jid, text, file string) bool {
	digits := WADigits(jid)
	if digits == "" {
		return false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || n == 0 {
		return false
	}
	id := -n // synthetic id: negative so it never collides with a positive Telegram chat id
	st := d.stateForWA(id, digits, jid)
	if st == nil {
		return false // not allow-listed for WhatsApp
	}
	if file != "" {
		d.handleIncomingFile(st, file, "", text)
		return true
	}
	d.handle(st, strings.TrimSpace(text))
	return true
}

// stateForWA returns (creating on first contact) the per-user session for a
// WhatsApp sender, or nil if the number isn't allow-listed. It records the jid so
// the central send helpers route replies over the sidecar.
func (d *Comp) stateForWA(id int64, handle, jid string) *userState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.byUser[id]; ok {
		st.waChat = jid
		return st
	}
	matched, entry, ok := d.lookupAllow("whatsapp", handle)
	if !ok {
		return nil
	}
	st := &userState{
		username: matched,
		entry:    entry,
		chatID:   id,
		platform: "whatsapp",
		waChat:   jid,
		backend:  d.agents.For("bridge", entry.Agent),
	}
	d.byUser[id] = st
	d.waMu.Lock()
	d.waChat[id] = jid
	d.waMu.Unlock()
	return st
}

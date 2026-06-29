package bridge

import "strings"

// HandleWhatsApp routes one inbound WhatsApp message from the legacy Node Baileys
// sidecar poll into the bridge, exactly like an allow-listed Telegram message.
// jid is the sender's chat jid (used for replies over the sidecar); returns true
// when the sender is allow-listed for WhatsApp (and was therefore consumed) so
// the poll can mark it read. This path is poll-driven (the sidecar has no push)
// and shares its session with the daemon WhatsApp transport via sessionKey, so a
// person is one conversation regardless of which WA source delivered the message.
// It will be removed once whatsmeow (the in-process WA transport) is paired and
// the sidecar is retired.
func (d *Comp) HandleWhatsApp(jid, text, file string) bool {
	digits := WADigits(jid)
	if digits == "" {
		return false
	}
	st := d.stateForSidecarWA(digits, jid)
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

// stateForSidecarWA returns (creating on first contact) the session for a sidecar
// WhatsApp sender, or nil if the number isn't allow-listed. It marks the session
// viaSidecar so replies go back over the Node sidecar.
func (d *Comp) stateForSidecarWA(digits, jid string) *userState {
	key := sessionKey("whatsapp", digits)
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.byUser[key]; ok {
		st.address = jid
		st.viaSidecar = true
		return st
	}
	matched, entry, ok := d.lookupAllow("whatsapp", digits)
	if !ok {
		return nil
	}
	st := &userState{
		username:   matched,
		entry:      entry,
		transport:  "whatsapp",
		address:    jid,
		viaSidecar: true,
		backend:    d.agents.For("bridge", entry.Agent),
	}
	d.byUser[key] = st
	return st
}

package bridge

// WhatsApp inbound is handled uniformly through the daemon subscribe stream now
// (whatsmeow → HandleEvent → stateFor in runtime.go, keyed by transport+number).
// The legacy Node Baileys sidecar path (HandleWhatsApp / stateForSidecarWA) was
// removed when the sidecar was retired — replies go out over the daemon too.

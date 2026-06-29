package bridge

import (
	"testing"

	"github.com/Zouriel/zcoms/client"
)

func testComp() *Comp {
	return &Comp{
		allow: Allowlist{
			"telegram|@alice":     {Role: RoleRead},
			"whatsapp|9607654321": {Role: RoleRead},
		},
		agents: AgentConfig{},
		byUser: map[string]*userState{},
	}
}

// A Telegram event creates a telegram-routed session keyed by user id.
func TestStateForTelegram(t *testing.T) {
	d := testComp()
	st := d.stateFor(client.Event{Sender: "@alice", UserID: 42, ChatID: 42, Address: "42"})
	if st == nil {
		t.Fatal("allow-listed telegram sender rejected")
	}
	if st.transport != "telegram" || st.address != "42" {
		t.Fatalf("telegram session route wrong: %+v", st.route())
	}
	if r := st.route(); r.viaSidecar || r.transport != "telegram" || r.address != "42" {
		t.Fatalf("route() wrong: %+v", r)
	}
	// Non-allow-listed sender is rejected.
	if d.stateFor(client.Event{Sender: "@bob", UserID: 7, ChatID: 7}) != nil {
		t.Fatal("non-allow-listed sender accepted")
	}
}

// A daemon WhatsApp event (whatsmeow) creates a whatsapp-routed session matched
// by number, replying over the daemon (not the sidecar).
func TestStateForWhatsApp(t *testing.T) {
	d := testComp()
	jid := "9607654321@s.whatsapp.net"
	st := d.stateFor(client.Event{Transport: "whatsapp", Address: jid, Sender: "Imdaah"})
	if st == nil {
		t.Fatal("allow-listed whatsapp sender rejected")
	}
	if r := st.route(); r.transport != "whatsapp" || r.address != jid || r.viaSidecar {
		t.Fatalf("whatsapp route wrong: %+v", r)
	}
}

// The legacy sidecar path and the daemon path share one session per number, and
// the reply route follows whichever source delivered the latest message.
func TestSidecarAndDaemonShareSession(t *testing.T) {
	d := testComp()
	jid := "9607654321@s.whatsapp.net"

	// First arrives via the Baileys sidecar: reply over the sidecar.
	sc := d.stateForSidecarWA("9607654321", jid)
	if sc == nil || !sc.route().viaSidecar {
		t.Fatalf("sidecar session not viaSidecar: %+v", sc)
	}

	// Same number now arrives via the daemon: same session object, route flips
	// to the daemon (viaSidecar=false).
	dm := d.stateFor(client.Event{Transport: "whatsapp", Address: jid})
	if dm != sc {
		t.Fatal("daemon WA created a separate session instead of sharing by number")
	}
	if dm.route().viaSidecar {
		t.Fatal("route did not flip to the daemon after a daemon-delivered message")
	}
}

func TestSessionKeyNamespacesTransports(t *testing.T) {
	if sessionKey("telegram", "123") == sessionKey("whatsapp", "123") {
		t.Fatal("telegram and whatsapp ids must not collide")
	}
	if sessionKey("", "5") != sessionKey("telegram", "5") {
		t.Fatal("empty transport must default to telegram")
	}
}

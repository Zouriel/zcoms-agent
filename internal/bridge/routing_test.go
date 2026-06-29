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
	if r := st.route(); r.transport != "telegram" || r.address != "42" {
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
	if r := st.route(); r.transport != "whatsapp" || r.address != jid {
		t.Fatalf("whatsapp route wrong: %+v", r)
	}

	// A second message from the same number refreshes the same session (the
	// reply address follows the latest inbound), keyed by number.
	st2 := d.stateFor(client.Event{Transport: "whatsapp", Address: jid, Sender: "Imdaah"})
	if st2 != st {
		t.Fatal("same WhatsApp number created a separate session instead of reusing one")
	}
}

// SetAllow takes effect live: a new allowlist that drops a principal evicts
// their active session so they're no longer served without a restart.
func TestSetAllowEvictsRemoved(t *testing.T) {
	d := testComp()
	jid := "9607654321@s.whatsapp.net"
	if d.stateFor(client.Event{Transport: "whatsapp", Address: jid}) == nil {
		t.Fatal("setup: allow-listed WA sender should get a session")
	}

	// Remove WhatsApp from the allowlist and push it live.
	d.SetAllow(Allowlist{"telegram|@alice": {Role: RoleRead}})

	// The evicted number is no longer served (existing session dropped, and a
	// fresh lookup fails).
	if d.stateFor(client.Event{Transport: "whatsapp", Address: jid}) != nil {
		t.Fatal("removed WhatsApp number still served after SetAllow")
	}
	// An added principal works immediately.
	d.SetAllow(Allowlist{"telegram|@alice": {Role: RoleRead}, "whatsapp|9607654321": {Role: RoleRead}})
	if d.stateFor(client.Event{Transport: "whatsapp", Address: jid}) == nil {
		t.Fatal("re-added WhatsApp number not served after SetAllow")
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

package bridge

import (
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms/client"
)

func testComp() *Comp {
	return &Comp{
		allow: Allowlist{
			"telegram|@alice":     {Role: RoleRead},
			"whatsapp|9607654321": {Role: RoleRead},
		},
		agentBrain: func(string, Backend) Brain { return Brain{Backend: BackendClaude} },
		byUser:     map[string]*userState{},
	}
}

// errand commands run against the injected in-process errands handler (errands
// no longer lives on its own errands.sock service), not a dead socket dial.
func TestErrandCommandUsesInProcessHandler(t *testing.T) {
	d := testComp()
	var got string
	d.errandCmd = func(text string) string { got = text; return "ok: " + text }
	if reply := d.errandCommand("errand yes 123"); reply != "ok: errand yes 123" {
		t.Fatalf("errand reply = %q", reply)
	}
	if got != "errand yes 123" {
		t.Fatalf("handler got %q, want the full command text", got)
	}
	// With no handler wired it degrades gracefully, no socket dial, no panic.
	d.errandCmd = nil
	if reply := d.errandCommand("errand list"); !strings.Contains(reply, "aren't available") {
		t.Fatalf("nil-handler reply = %q", reply)
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

// The per-turn speaker line names the actual requester and marks a non-owner as
// NOT the owner, so a chat session can no longer mistake the wife for the owner.
func TestSpeakerLineDistinguishesOwner(t *testing.T) {
	d := testComp()
	d.settings = Settings{MainUser: "@ZourielCorbet"}

	owner := d.speakerLine(&userState{username: "@ZourielCorbet", transport: "telegram", address: "1"})
	if !strings.Contains(owner, "@ZourielCorbet") || strings.Contains(owner, "NOT the owner") {
		t.Fatalf("owner should be framed as the owner: %q", owner)
	}

	wife := d.speakerLine(&userState{username: "@reiniellle", transport: "telegram", address: "2"})
	if !strings.Contains(wife, "@reiniellle") || !strings.Contains(wife, "NOT the owner") {
		t.Fatalf("non-owner should be named and marked NOT the owner: %q", wife)
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

// A `chat` session must run on the bridge persona: the row whose seed it already
// uses, and whose backend/model the console shows for it. Asking for a task name
// no persona provides is what made a console-set codex chat run on claude.
func TestChatSessionRunsOnTheBridgePersona(t *testing.T) {
	d := testComp()
	d.client = &client.Client{} // sends fail (dead socket) and are ignored
	var asked []string
	d.agentBrain = func(persona string, _ Backend) Brain {
		asked = append(asked, persona)
		return Brain{Backend: BackendCodex}
	}

	st := d.stateFor(client.Event{Sender: "@alice", UserID: 42, ChatID: 42, Address: "42"})
	if st == nil {
		t.Fatal("setup: allow-listed sender should get a session")
	}
	// A session with no location resolves as the bridge persona.
	if got := d.brain(st.persona, ""); got.Backend != BackendCodex {
		t.Fatalf("unscoped session brain = %+v, want the persona's codex", got)
	}
	d.startChat(st)
	if st.persona != personas.Bridge {
		t.Fatalf("chat session persona = %q, want %q", st.persona, personas.Bridge)
	}
	d.brain(st.persona, "") // what the next turn would resolve
	if last := asked[len(asked)-1]; last != personas.Bridge {
		t.Fatalf("chat turn resolved persona %q, want %q", last, personas.Bridge)
	}
	if asked[0] != personas.Bridge {
		t.Fatalf("unscoped session resolved persona %q, want %q", asked[0], personas.Bridge)
	}
}

// A group call from an allow-listed sender must never open a 1:1 session: the
// bridge leaves Group events to the groups handler.
func TestHandleEventIgnoresGroupCalls(t *testing.T) {
	d := testComp()
	ev := client.Event{Group: true, Sender: "@alice", UserID: 42, ChatID: -100, Address: "-100", Text: "hi", Call: "mention"}
	if d.HandleEvent(ev) {
		t.Fatal("bridge handled a group event")
	}
	if len(d.byUser) != 0 {
		t.Fatalf("bridge opened %d session(s) for a group event", len(d.byUser))
	}
}

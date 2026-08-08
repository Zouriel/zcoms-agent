package reminders

import (
	"strings"
	"testing"

	"github.com/Zouriel/zcoms/client"
)

// An allow-listed person messages from a handle, but the directory knows them by
// name. Contact resolution matches names and aliases only, never the handle, so a
// request from @Zoella99 was attributed to "Zoella" and the reminder read "Zoella
// needs you to pick her up" when she is Shanna in contacts.
func TestDisplayNameMapsHandleToContact(t *testing.T) {
	fc := &fakeClient{contacts: []client.Contact{
		{ID: 2, Name: "Shanna ❤️", Telegram: "@Zoella99"},
		{ID: 1, Name: "Raani ❤️", Telegram: "@reiniellle"},
	}}
	d := &Comp{client: fc}

	got := d.displayName(Requester{Transport: "telegram", Handle: "@Zoella99", Name: "@Zoella99"})
	if got != "Shanna ❤️" {
		t.Fatalf("displayName = %q, want the contact name", got)
	}

	// Case and the leading @ must not matter.
	if got := d.displayName(Requester{Transport: "telegram", Handle: "zoella99", Name: "zoella99"}); got != "Shanna ❤️" {
		t.Fatalf("displayName(bare, lowercased) = %q", got)
	}

	// An unknown handle keeps whatever the session gave us.
	if got := d.displayName(Requester{Transport: "telegram", Handle: "@stranger", Name: "@stranger"}); got != "@stranger" {
		t.Fatalf("displayName(unknown) = %q, want it left alone", got)
	}

	// The owner refers to themselves as "you"; don't rewrite that.
	if got := d.displayName(Requester{Transport: "telegram", Handle: "@ZourielCorbet", Name: "you", Owner: true}); got != "you" {
		t.Fatalf("displayName(owner) = %q, want you", got)
	}
}

// The extraction prompt has to tell the model whose "me" it is reading, using the
// asker's real name, or the task keeps their first person and the confirmation
// reads "I'll remind Zouriel to come pick me up".
func TestInterpretPromptNamesTheAsker(t *testing.T) {
	p := interpretPrompt(Requester{Name: "Shanna ❤️"}, "remind Zouriel to come pick me up at 11:45")
	for _, want := range []string{"Shanna ❤️", "PERSON BEING REMINDED"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

// The `remind` keyword has to be recognisable to the router, which gives it
// priority over any conversation the owner has open.
func TestIsCommand(t *testing.T) {
	yes := []string{
		"remind Raani to take her father's report",
		"Remind me to eat",
		"reminders",
		"remind list",
		"  reminder Sara about the invoice",
	}
	no := []string{"", "reminding him is pointless", "lineup", "what's on today", "remindful"}
	for _, s := range yes {
		if !IsCommand(s) {
			t.Errorf("IsCommand(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsCommand(s) {
			t.Errorf("IsCommand(%q) = true, want false", s)
		}
	}
}

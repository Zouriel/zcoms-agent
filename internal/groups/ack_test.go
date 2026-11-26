package groups

import (
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

func TestParseUnderstoodAck(t *testing.T) {
	u := ParseUnderstood("TASK: Find why login fails\nLANG: Dhivehi\nWORK: long\nACK: Login code balaigen dhen kiyaidheyn.")
	if !u.Long || u.Ack != "Login code balaigen dhen kiyaidheyn." {
		t.Fatalf("long work lost its ack: %+v", u)
	}
	if u := ParseUnderstood("TASK: What is 2+2?\nWORK: quick\nACK: On it."); u.Ack != "" {
		t.Fatalf("quick work kept an ack: %+v", u)
	}
	if u := ParseUnderstood("NONE: says thanks\nWORK: long\nACK: On it."); u.Ack != "" {
		t.Fatalf("non-question kept an ack: %+v", u)
	}
}

func TestLongWorkAcknowledgesFirst(t *testing.T) {
	c := &fakeComms{}
	h := newHandler(c, func(r RunRequest) (string, error) {
		if len(c.sends) != 1 {
			t.Errorf("work pass started with %d messages sent, want the ack first", len(c.sends))
		}
		return "The login check rejects expired tokens.", nil
	})
	work := h.Run
	h.Run = func(r RunRequest) (string, error) {
		if r.Persona == UnderstandPersona {
			return "TASK: Why does login fail?\nLANG: English\nWORK: long\nACK: On it, checking the login code first.", nil
		}
		return work(r)
	}
	call, _ := CallFromEvent(groupEvent("-100", "42"))
	h.Process(store.GroupPolicy{Kind: store.GroupGeneral}, call)
	if len(c.sends) != 2 {
		t.Fatalf("sends = %+v, want ack then result", c.sends)
	}
	if c.sends[0].text != "On it, checking the login code first." || c.sends[0].replyTo != "42" || c.sends[1].replyTo != "42" {
		t.Fatalf("sends = %+v", c.sends)
	}
}

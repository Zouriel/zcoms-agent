package groups

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// adminComms is fakeComms plus the Admin side, recording each call.
type adminComms struct {
	fakeComms
	kept      []client.KeepOptions
	pins      []int64
	unpins    []int64
	edits     map[int64]string
	editErr   map[int64]error
	disappear []time.Duration
	timer     time.Duration
	nextID    int64
}

func (a *adminComms) Edit(_ string, id int64, text string) (client.Response, error) {
	if err := a.editErr[id]; err != nil {
		return client.Response{}, err
	}
	if a.edits == nil {
		a.edits = map[int64]string{}
	}
	a.edits[id] = text
	return client.Response{OK: true, MessageID: id}, nil
}

func (a *adminComms) Pin(_ string, id int64, _ client.PinOptions) (client.Response, error) {
	a.pins = append(a.pins, id)
	return client.Response{OK: true}, nil
}

func (a *adminComms) Unpin(_ string, id int64) (client.Response, error) {
	a.unpins = append(a.unpins, id)
	return client.Response{OK: true}, nil
}

func (a *adminComms) SendKeep(to, text string, o client.KeepOptions) (client.Response, error) {
	a.kept = append(a.kept, o)
	a.sends = append(a.sends, sent{transport: "telegram", to: to, text: text, replyTo: o.ReplyTo, thread: o.Thread})
	return client.Response{OK: true, MessageID: 500}, nil
}

func (a *adminComms) SetDisappear(_ string, d time.Duration) (client.Response, error) {
	a.disappear = append(a.disappear, d)
	return client.Response{OK: true}, nil
}

func (a *adminComms) Disappear(string) (time.Duration, error) { return a.timer, nil }

// SendWith gives each plain send an id so EDIT has something to find.
func (a *adminComms) SendWith(transport, to, text string, o client.SendOptions) (client.Response, error) {
	a.nextID += 100
	a.sends = append(a.sends, sent{transport: transport, to: to, text: text, replyTo: o.ReplyTo, thread: o.Thread})
	return client.Response{OK: true, MessageID: a.nextID}, nil
}

// manageCall runs one call whose work pass answers out, from the owner or
// sender 77 with the given role, replying to message quoted ("" for none).
func manageCall(t *testing.T, h *Handler, out string, owner bool, quoted string) {
	t.Helper()
	h.Run = func(r RunRequest) (string, error) {
		if r.Persona == UnderstandPersona {
			return "TASK: do it", nil
		}
		return out, nil
	}
	ev := groupEvent("-100", "70")
	ev.SenderIsOwner, ev.ReplyToID = owner, quoted
	call, _ := CallFromEvent(ev)
	h.Process(store.GroupPolicy{Kind: store.GroupGeneral}, call)
}

func manageHandler(roles map[string]string) (*Handler, *adminComms) {
	a := &adminComms{}
	h := newHandler(&a.fakeComms, nil)
	h.Comms = a
	pol := h.Policies.(fakePolicies)
	for m, r := range roles {
		pol["role|telegram|-100|"+m] = store.GroupPolicy{Kind: r}
	}
	return h, a
}

func TestSplitManageDirectives(t *testing.T) {
	text, d := SplitDirectives("Here you go.\nPIN: reply\nUNPIN: quoted\nEDIT: last\nDISAPPEAR: 7d")
	if text != "Here you go." || d.Pin != "reply" || !d.Unpin || !d.Edit || d.Disappear != "7d" {
		t.Fatalf("text=%q d=%+v", text, d)
	}
	_, d = SplitDirectives("x\nPIN: everything\nEDIT: no\nDISAPPEAR: none")
	if d.Pin != "" || d.Edit || d.Disappear != "" {
		t.Fatalf("unknown values kept: %+v", d)
	}
}

func TestPinReplyKeepsAndQuotes(t *testing.T) {
	h, a := manageHandler(nil)
	manageCall(t, h, "Standup is at 9.\nPIN: reply", true, "")
	if len(a.kept) != 1 || !a.kept[0].Pin || a.kept[0].ReplyTo != "70" {
		t.Fatalf("kept = %+v", a.kept)
	}
	if a.sends[len(a.sends)-1].text != "Standup is at 9." {
		t.Fatalf("sends = %+v", a.sends)
	}
}

func TestReadMemberCannotPinOrEdit(t *testing.T) {
	h, a := manageHandler(nil) // sender 77 is read by default
	manageCall(t, h, "Sure.\nPIN: reply\nEDIT: last", false, "")
	if len(a.kept) != 0 || len(a.edits) != 0 {
		t.Fatalf("read member managed: kept=%v edits=%v", a.kept, a.edits)
	}
	got := a.sends[len(a.sends)-1].text
	if !strings.Contains(got, "Sure.") || strings.Count(got, noteManageRole) != 1 {
		t.Fatalf("reply = %q", got)
	}
}

func TestPinQuotedWarnsInDisappearingGroup(t *testing.T) {
	h, a := manageHandler(map[string]string{"77": store.RoleWrite})
	a.timer = 24 * time.Hour
	manageCall(t, h, "Pinned it.\nPIN: quoted", false, "42")
	if len(a.pins) != 1 || a.pins[0] != 42 {
		t.Fatalf("pins = %v", a.pins)
	}
	if got := a.sends[len(a.sends)-1].text; !strings.Contains(got, "will still disappear") {
		t.Fatalf("reply = %q", got)
	}

	// Without a quoted message there is nothing to pin.
	manageCall(t, h, "ok\nUNPIN: quoted", false, "")
	if len(a.unpins) != 0 || !strings.Contains(a.sends[len(a.sends)-1].text, noteNoQuoted) {
		t.Fatalf("unpins=%v reply=%q", a.unpins, a.sends[len(a.sends)-1].text)
	}
}

func TestDisappearIsOwnerOnly(t *testing.T) {
	h, a := manageHandler(map[string]string{"77": store.RoleWrite})
	manageCall(t, h, "Done.\nDISAPPEAR: 7d", false, "")
	if len(a.disappear) != 0 || !strings.Contains(a.sends[len(a.sends)-1].text, noteDisappearRole) {
		t.Fatalf("writer changed the timer: %v", a.disappear)
	}
	manageCall(t, h, "Done.\nDISAPPEAR: 2w", true, "")
	if len(a.disappear) != 1 || a.disappear[0] != 14*24*time.Hour {
		t.Fatalf("disappear = %v", a.disappear)
	}
	if got := a.sends[len(a.sends)-1].text; !strings.Contains(got, "2 weeks") {
		t.Fatalf("reply = %q", got)
	}
	manageCall(t, h, "Done.\nDISAPPEAR: 5x", true, "")
	if len(a.disappear) != 1 || !strings.Contains(a.sends[len(a.sends)-1].text, "did not change") {
		t.Fatalf("bad timer applied: %v", a.disappear)
	}
}

func TestEditReplacesLastAnswer(t *testing.T) {
	h, a := manageHandler(nil)
	manageCall(t, h, "The meeting is at 10.", true, "")
	first := a.nextID
	manageCall(t, h, "The meeting is at 11.\nEDIT: last", true, "")
	if a.edits[first] != "The meeting is at 11." {
		t.Fatalf("edits = %v (first answer id %d)", a.edits, first)
	}
	if got := a.sends[len(a.sends)-1].text; got != "Edited." {
		t.Fatalf("confirmation = %q", got)
	}

	// A quoted message that is not mine fails; my last answer is used instead.
	a.editErr = map[int64]error{9: errors.New("message can't be edited")}
	manageCall(t, h, "The meeting is at noon.\nEDIT: last", true, "9")
	if a.edits[first] != "The meeting is at noon." {
		t.Fatalf("fallback edit = %v", a.edits)
	}
}

func TestEditWithNothingToEditPostsAgain(t *testing.T) {
	h, a := manageHandler(nil)
	manageCall(t, h, "Fixed text.\nEDIT: last", true, "")
	if len(a.edits) != 0 {
		t.Fatalf("edits = %v", a.edits)
	}
	if got := a.sends[len(a.sends)-1].text; !strings.Contains(got, "Fixed text.") || !strings.Contains(got, noteNoEdit) {
		t.Fatalf("reply = %q", got)
	}
}

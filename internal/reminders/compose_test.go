package reminders

import (
	"strings"
	"testing"
)

// The template fallback voice must be natural: it mentions the task, carries no
// robotic "reply done/not yet" hint, and escalates encouragement on later nudges.
func TestTemplateLine(t *testing.T) {
	base := ComposeCtx{Task: "buy a rose", Self: true}

	for _, kind := range []MsgKind{MsgPre, MsgConfirm, MsgNudge, MsgMissed, MsgDone} {
		got := templateLine(kind, base)
		if strings.TrimSpace(got) == "" {
			t.Fatalf("%s produced empty text", kind)
		}
		if strings.Contains(strings.ToLower(got), "reply") {
			t.Errorf("%s line has a robotic reply hint: %q", kind, got)
		}
	}

	if !strings.Contains(templateLine(MsgMissed, base), "hand") {
		t.Error("missed line should gently offer help")
	}
	// No em-dashes in any template line (they read as robotic/AI).
	for _, kind := range []MsgKind{MsgPre, MsgConfirm, MsgNudge, MsgSnoozeAck, MsgMissed, MsgDone} {
		if strings.ContainsAny(templateLine(kind, ComposeCtx{Task: "x", Gap: "5m", TargetName: "Sara"}), "—–") {
			t.Errorf("%s template contains a dash", kind)
		}
	}

	// Snooze ack uses the gap.
	if got := templateLine(MsgSnoozeAck, ComposeCtx{Task: "x", Gap: "5m"}); !strings.Contains(got, "5m") {
		t.Errorf("snooze ack should mention the gap: %q", got)
	}

	// Third-party messages greet the contact by name.
	got := templateLine(MsgPre, ComposeCtx{Task: "send the invoice", TargetName: "Sara"})
	if !strings.Contains(got, "Sara") {
		t.Errorf("third-party pre should greet by name: %q", got)
	}

	// Escalated nudge (attempt 3+) reads more encouraging/different from the first.
	first := templateLine(MsgNudge, ComposeCtx{Task: "buy a rose", Attempt: 1})
	later := templateLine(MsgNudge, ComposeCtx{Task: "buy a rose", Attempt: 4})
	if first == later {
		t.Error("a later nudge should escalate vs the first")
	}
}

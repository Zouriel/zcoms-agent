package agentd

import (
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/reminders"
)

// A caller on agent.sock is a program, not a person reading a chat. The module
// answers refusals in prose, so a relayed cancel of a reminder that does not
// exist came back ok and the MCP bridge reported "cancelled: true".
func TestRemindFailuresAreErrorsOnTheSocket(t *testing.T) {
	a := testAgent(t)
	a.Reminders = reminders.New(nil, a.Store, "@owner", 1, nil, nil)

	for _, line := range []string{"remind cancel 999999", "remind edit 999999 task nope", "remind edit"} {
		out, err := a.dispatch(line)
		if err == nil {
			t.Errorf("%q should be an error on the socket, got reply %q", line, out)
		}
	}

	// A reply that reports nothing to show is not a failure.
	if _, err := a.dispatch("remind list"); err != nil {
		t.Fatalf("an empty list is not an error: %v", err)
	}
}

func TestReminderFailedPredicate(t *testing.T) {
	failing := []string{
		"⚠️ that reminder isn't yours to edit",
		"No reminder #12.",
		"Usage: remind edit <id> task <new task>",
	}
	for _, s := range failing {
		if !reminders.Failed(s) {
			t.Errorf("%q should read as a failure", s)
		}
	}

	passing := []string{
		"No active reminders.",
		"No reminders at all.",
		"✅ Reminder #3 set. I'll remind you to water the plants.",
		"🗑️ Reminder #3 cancelled.",
		"✏️ Reminder #3 updated, now due today 5:00 PM.",
		"#3 → you: water the plants\n      due today 5:00 PM",
	}
	for _, s := range passing {
		if reminders.Failed(s) {
			t.Errorf("%q should read as success: %s", s, strings.TrimSpace(s))
		}
	}
}

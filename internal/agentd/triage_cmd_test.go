package agentd

import (
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// Every word that was not a known subcommand used to fall through to the schedule
// setter, so one mistyped or unsupported call ("triage digest") silently replaced
// the schedule with itself and left the scheduler with nothing valid to run on.
func TestTriageRejectsUnknownSubcommand(t *testing.T) {
	a := testAgent(t)
	if err := a.Store.SetSetting(store.Owner, "triage_schedule", "1h"); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	for _, bad := range []string{"digest", "summary", "whatever"} {
		if _, err := a.triageCmd([]string{bad}); err == nil {
			t.Errorf("triage %q should be refused", bad)
		}
		got, _ := a.Store.GetSetting("triage_schedule")
		if got != "1h" {
			t.Fatalf("triage %q overwrote the schedule with %q", bad, got)
		}
	}

	if _, err := a.triageCmd([]string{"digest"}); err == nil || !strings.Contains(err.Error(), "30m") {
		t.Fatalf("the refusal should list the valid schedules, got: %v", err)
	}
}

// A real schedule still sets.
func TestTriageAcceptsValidSchedule(t *testing.T) {
	a := testAgent(t)
	if _, err := a.triageCmd([]string{"6h"}); err != nil {
		t.Fatalf("set 6h: %v", err)
	}
	if got, _ := a.Store.GetSetting("triage_schedule"); got != "6h" {
		t.Fatalf("schedule = %q, want 6h", got)
	}
	if _, err := a.triageCmd([]string{"twice-daily"}); err != nil {
		t.Fatalf("set twice-daily: %v", err)
	}
	if got, _ := a.Store.GetSetting("triage_schedule"); got != "twice-daily" {
		t.Fatalf("schedule = %q, want twice-daily", got)
	}
}

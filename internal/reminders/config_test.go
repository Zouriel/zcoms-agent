package reminders

import (
	"strings"
	"testing"
	"time"
)

func TestConfigFromMap(t *testing.T) {
	c := configFromMap(map[string]string{
		"reminder.enabled":        "false",
		"reminder.voice":          "simple",
		"reminder.followup_mins":  "8",
		"reminder.max_nudges":     "3",
		"reminder.first_nudge_mins": "bad", // invalid → keep default
	})
	if c.Enabled {
		t.Error("enabled=false not parsed")
	}
	if c.Voice != "simple" || c.agentVoice() {
		t.Errorf("voice: %+v", c)
	}
	if c.FollowupMins != 8 || c.MaxNudges != 3 {
		t.Errorf("ints: %+v", c)
	}
	if c.FirstNudgeMins != DefaultConfig().FirstNudgeMins {
		t.Errorf("invalid int should keep default, got %d", c.FirstNudgeMins)
	}
}

func TestApplyConfig(t *testing.T) {
	c := Config{FirstNudgeMins: 60, FollowupMins: 15, DeadlineLeadMins: 10, DeadlineAfterMins: 25, MaxNudges: 12}

	// Non-explicit open task → owner default gaps.
	d := applyConfig(Decision{Kind: "oneoff"}, c)
	if d.PreDelay != time.Hour || d.PostGap != 15*time.Minute {
		t.Fatalf("open: %v / %v", d.PreDelay, d.PostGap)
	}
	// Deadline → configured lead/after.
	d = applyConfig(Decision{Kind: "oneoff", DeadlineBound: true}, c)
	if d.PreDelay != 10*time.Minute || d.PostGap != 25*time.Minute {
		t.Fatalf("deadline: %v / %v", d.PreDelay, d.PostGap)
	}
	// Explicit task → keep its own pre, take the configured follow-up.
	d = applyConfig(Decision{Kind: "oneoff", Explicit: true, PreDelay: 2 * time.Minute}, c)
	if d.PreDelay != 2*time.Minute || d.PostGap != 15*time.Minute {
		t.Fatalf("explicit: %v / %v", d.PreDelay, d.PostGap)
	}
	// Recurring is event-anchored — untouched.
	d = applyConfig(Decision{Kind: "recurring", PreDelay: 5 * time.Minute}, c)
	if d.PreDelay != 5*time.Minute {
		t.Fatalf("recurring touched: %v", d.PreDelay)
	}
}

func TestExplicitDetection(t *testing.T) {
	now := time.Date(2026, 6, 30, 10, 0, 0, 0, time.Local)
	h := heuristic{}
	explicit := []string{"the 3pm sync", "water the plants in 20 minutes", "call mom tomorrow", "standup every weekday at 9am"}
	vague := []string{"buy a rose", "call the plumber", "reply to the email"}
	for _, task := range explicit {
		if !h.Classify(task, now).Explicit {
			t.Errorf("%q should be Explicit", task)
		}
	}
	for _, task := range vague {
		if h.Classify(task, now).Explicit {
			t.Errorf("%q should NOT be Explicit", task)
		}
	}
}

// Settings written via the command are read back live (no restart).
func TestSettingsCommandLive(t *testing.T) {
	clf := &fakeClassifier{}
	d, _, _ := newTestComp(t, clf)
	owner := Requester{Owner: true}
	nonOwner := Requester{Owner: false}

	if got := d.settingsReply(nonOwner, []string{"followup_mins", "5"}); !strings.Contains(got, "owner") {
		t.Fatalf("non-owner should be rejected: %q", got)
	}
	if got := d.settingsReply(owner, []string{"followup_mins", "9"}); !strings.HasPrefix(got, "✅") {
		t.Fatalf("set failed: %q", got)
	}
	if d.cfg().FollowupMins != 9 {
		t.Fatalf("live read = %d, want 9", d.cfg().FollowupMins)
	}
	if got := d.settingsReply(owner, []string{"nope", "1"}); !strings.Contains(got, "unknown") {
		t.Fatalf("bad key should be rejected: %q", got)
	}
}

package reminders

import (
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// Reminder behaviour knobs, stored as scalar settings (agent.db) so the console
// can tweak them and the engine reads them LIVE — no restart, no reload. Defaults
// match the constants the feature shipped with.
const (
	keyEnabled       = "reminder.enabled"
	keyVoice         = "reminder.voice"
	keyFirstNudge    = "reminder.first_nudge_mins"
	keyFollowup      = "reminder.followup_mins"
	keyDeadlineLead  = "reminder.deadline_lead_mins"
	keyDeadlineAfter = "reminder.deadline_after_mins"
	keyMaxNudges     = "reminder.max_nudges"
)

// Config is the live, owner-tunable reminder behaviour.
type Config struct {
	Enabled           bool   `json:"enabled"`             // master switch
	Voice             string `json:"voice"`               // "agent" (model-written) | "simple" (templates)
	FirstNudgeMins    int    `json:"first_nudge_mins"`    // default lead when no time is given
	FollowupMins      int    `json:"followup_mins"`       // gap before the "did you do it?" check-in
	DeadlineLeadMins  int    `json:"deadline_lead_mins"`  // pre-remind this long before a deadline event
	DeadlineAfterMins int    `json:"deadline_after_mins"` // ask how it went this long after the event
	MaxNudges         int    `json:"max_nudges"`          // chase cap for an until-done task
}

// DefaultConfig is the shipped behaviour.
func DefaultConfig() Config {
	return Config{
		Enabled: true, Voice: "agent",
		FirstNudgeMins: 60, FollowupMins: 15,
		DeadlineLeadMins: 15, DeadlineAfterMins: 20, MaxNudges: 12,
	}
}

func (c Config) firstNudge() time.Duration    { return time.Duration(c.FirstNudgeMins) * time.Minute }
func (c Config) followup() time.Duration      { return time.Duration(c.FollowupMins) * time.Minute }
func (c Config) deadlineLead() time.Duration  { return time.Duration(c.DeadlineLeadMins) * time.Minute }
func (c Config) deadlineAfter() time.Duration { return time.Duration(c.DeadlineAfterMins) * time.Minute }
func (c Config) agentVoice() bool             { return c.Voice != "simple" }

func configFromMap(m map[string]string) Config {
	c := DefaultConfig()
	if v, ok := m[keyEnabled]; ok {
		c.Enabled = !(v == "false" || v == "0" || v == "off")
	}
	if v := strings.TrimSpace(m[keyVoice]); v == "simple" || v == "agent" {
		c.Voice = v
	}
	c.FirstNudgeMins = posIntOr(m[keyFirstNudge], c.FirstNudgeMins)
	c.FollowupMins = posIntOr(m[keyFollowup], c.FollowupMins)
	c.DeadlineLeadMins = posIntOr(m[keyDeadlineLead], c.DeadlineLeadMins)
	c.DeadlineAfterMins = posIntOr(m[keyDeadlineAfter], c.DeadlineAfterMins)
	c.MaxNudges = posIntOr(m[keyMaxNudges], c.MaxNudges)
	return c
}

func posIntOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

// LoadConfig reads the live reminder config from agent.db (defaults on any error).
func LoadConfig(st *store.Store) Config {
	m, err := st.ListSettings()
	if err != nil {
		return DefaultConfig()
	}
	return configFromMap(m)
}

// configKeys is the set of settable reminder knobs (the console setter validates
// against this so it can't write arbitrary settings).
var configKeys = map[string]bool{
	keyEnabled: true, keyVoice: true, keyFirstNudge: true, keyFollowup: true,
	keyDeadlineLead: true, keyDeadlineAfter: true, keyMaxNudges: true,
}

// SettingKey returns the full settings key for a short config field name (the
// console posts e.g. {field:"followup_mins"}), or "" if unknown.
func SettingKey(field string) string {
	k := "reminder." + strings.TrimSpace(field)
	if configKeys[k] {
		return k
	}
	if configKeys[field] { // already-qualified
		return field
	}
	return ""
}

func (d *Comp) cfg() Config { return LoadConfig(d.store) }

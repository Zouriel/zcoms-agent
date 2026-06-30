package reminders

import (
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

// The agent owns nearly all the behaviour now, so settings shrink to a master
// switch, a safety cap on total runs (a runaway agent can't pester forever), and
// how long a single run waits for the reply before reacting. Stored as scalar
// settings in agent.db and read LIVE — no restart.
const (
	keyEnabled   = "reminder.enabled"
	keyMaxRuns   = "reminder.max_runs"
	keyReplyWait = "reminder.reply_wait_mins"
)

// Config is the live, owner-tunable reminder behaviour.
type Config struct {
	Enabled      bool `json:"enabled"`
	MaxRuns      int  `json:"max_runs"`        // safety cap on total runs per reminder
	ReplyWaitMins int `json:"reply_wait_mins"` // how long a run waits for the reply
}

func DefaultConfig() Config {
	return Config{Enabled: true, MaxRuns: 40, ReplyWaitMins: 10}
}

func (c Config) replyWait() time.Duration { return time.Duration(c.ReplyWaitMins) * time.Minute }

func configFromMap(m map[string]string) Config {
	c := DefaultConfig()
	if v, ok := m[keyEnabled]; ok {
		c.Enabled = !(v == "false" || v == "0" || v == "off")
	}
	c.MaxRuns = posIntOr(m[keyMaxRuns], c.MaxRuns)
	c.ReplyWaitMins = posIntOr(m[keyReplyWait], c.ReplyWaitMins)
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

func (d *Comp) cfg() Config { return LoadConfig(d.store) }

var configKeys = map[string]bool{keyEnabled: true, keyMaxRuns: true, keyReplyWait: true}

// SettingKey maps a short config field name to its full settings key, or "".
func SettingKey(field string) string {
	k := "reminder." + strings.TrimSpace(field)
	if configKeys[k] {
		return k
	}
	if configKeys[field] {
		return field
	}
	return ""
}

// LiveBackend resolves the reminders task's agent backend from the persona rows
// live (so a console backend change applies with no restart).
func LiveBackend(st *store.Store) runner.Backend {
	cfg := runner.AgentConfig{Tasks: map[string]runner.Backend{}}
	if ps, err := st.ListPersonas(); err == nil {
		for _, p := range ps {
			cfg.Tasks[p.Key] = runner.Backend(p.Backend)
		}
	}
	return cfg.For("reminders", "")
}

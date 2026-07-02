package morning

import (
	"path/filepath"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

// KeyEnabled is the settings key for the master on/off toggle (surfaced live in
// the console). Absent means enabled, so a fresh install runs the briefing.
const KeyEnabled = "morning.enabled"

// Config is the live, owner-tunable morning-agent behaviour. It is intentionally
// tiny: the trigger time is a fixed early-morning slot and everything else is the
// agent's judgement.
type Config struct {
	Enabled bool `json:"enabled"`
}

// DefaultConfig is the behaviour before any setting is written.
func DefaultConfig() Config { return Config{Enabled: true} }

// LoadConfig reads the live morning config from agent.db (defaults on any error).
func LoadConfig(st *store.Store) Config {
	c := DefaultConfig()
	if v, err := st.GetSetting(KeyEnabled); err == nil && v != "" {
		c.Enabled = !(v == "false" || v == "0" || v == "off")
	}
	return c
}

// NewAgentTurn builds the real morning-agent turn: one RunAgent call on the live
// morning backend in a staging dir. resumeID threads the turns of a briefing so
// the agent remembers what it already said this morning.
func NewAgentTurn(backendFn func() runner.Backend) AgentTurn {
	dir := ""
	if d, err := runner.DefaultAppDir(); err == nil {
		dir = filepath.Join(d, "morning-staging")
	}
	return func(prompt, resumeID string) (string, string, error) {
		backend := backendFn()
		if backend == "" || dir == "" {
			return "", "", errNoBackend
		}
		if err := ensureDir(dir); err != nil {
			return "", "", err
		}
		res, err := runner.RunAgent(backend, dir, prompt, resumeID, runner.RoleRead, false)
		return res.Text, res.SessionID, err
	}
}

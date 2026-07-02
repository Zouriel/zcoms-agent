package runner

import (
	"os/exec"
)

// AgentConfig selects which backend handles which task (agents.json).
//
//	{
//	  "default": "claude",          // all bridge work / anything unspecified
//	  "tasks": { "triage": "codex" } // per-task overrides
//	}
type AgentConfig struct {
	Default Backend            `json:"default"`
	Tasks   map[string]Backend `json:"tasks,omitempty"`
}

const agentsFile = "agents.json"

// SessionTypes are the agents.json task keys, one per kind of agent session, so
// each can run a different backend (`zc agents set <type> <claude|codex>`).
var SessionTypes = []string{"bridge", "triage", "errands", "reminders", "morning", "reschedule"}

// IsSessionType reports whether s is a known session type.
func IsSessionType(s string) bool {
	for _, t := range SessionTypes {
		if t == s {
			return true
		}
	}
	return false
}

// AgentAvailable reports whether the backend's CLI is installed.
func AgentAvailable(b Backend) bool {
	switch b.normalize() {
	case BackendCodex:
		_, err := exec.LookPath("codex")
		return err == nil
	default:
		_, err := exec.LookPath("claude")
		return err == nil
	}
}

// AvailableAgents returns the installed backends, claude first.
func AvailableAgents() []Backend {
	var out []Backend
	if AgentAvailable(BackendClaude) {
		out = append(out, BackendClaude)
	}
	if AgentAvailable(BackendCodex) {
		out = append(out, BackendCodex)
	}
	return out
}

// DefaultAgent prefers claude, then codex, else "" (agent mode unavailable).
func DefaultAgent() Backend {
	if AgentAvailable(BackendClaude) {
		return BackendClaude
	}
	if AgentAvailable(BackendCodex) {
		return BackendCodex
	}
	return ""
}

// For resolves the backend for a task: an explicit override wins, then the
// per-task setting, then the default, then any installed agent. Returns "" only
// when no agent is installed at all. Configured-but-uninstalled agents are
// skipped so the daemon keeps working.
func (c AgentConfig) For(task string, override Backend) Backend {
	var candidates []Backend
	if override != "" {
		candidates = append(candidates, override)
	}
	if task != "" {
		if t, ok := c.Tasks[task]; ok && t != "" {
			candidates = append(candidates, t)
		}
	}
	if c.Default != "" {
		candidates = append(candidates, c.Default)
	}
	candidates = append(candidates, BackendClaude, BackendCodex)

	for _, b := range candidates {
		if AgentAvailable(b) {
			return b.normalize()
		}
	}
	return ""
}

// LoadOrSeedAgents reads agents.json, seeding it with the detected default on
// first run.
func LoadOrSeedAgents() (AgentConfig, string, error) {
	path, _ := configFilePath()
	var cfg AgentConfig
	found, err := loadSection("agents", &cfg)
	if err != nil {
		return AgentConfig{}, path, err
	}
	if !found {
		cfg = AgentConfig{Default: DefaultAgent(), Tasks: map[string]Backend{}}
		_ = saveSection("agents", cfg)
		return cfg, path, nil
	}
	if cfg.Tasks == nil {
		cfg.Tasks = map[string]Backend{}
	}
	if cfg.Default == "" {
		cfg.Default = DefaultAgent()
	}
	return cfg, path, nil
}

// SaveAgents writes the agents section of config.json.
func SaveAgents(cfg AgentConfig) (string, error) {
	path, _ := configFilePath()
	return path, saveSection("agents", cfg)
}

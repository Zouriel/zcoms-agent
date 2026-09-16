package runner

import (
	"os/exec"
)

// AgentConfig selects which backend handles which task. It is the legacy
// agents.json shape, kept for the one-time import into agent.db: at runtime the
// persona rows are the source of truth and the task keys ARE persona keys, so
// resolve through personas.Brain rather than building a config here.
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

// AgentAvailable reports whether the backend's CLI is installed. It resolves the
// binary exactly as a run does (PATH, then ~/.local/bin), because a systemd user
// service gets a minimal PATH without ~/.local/bin: a PATH-only probe reported
// claude missing and silently rerouted every persona to codex.
func AgentAvailable(b Backend) bool {
	name := string(BackendClaude)
	if b.normalize() == BackendCodex {
		name = string(BackendCodex)
	}
	_, err := exec.LookPath(agentBin(name))
	return err == nil
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

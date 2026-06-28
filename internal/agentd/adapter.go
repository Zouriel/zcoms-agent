package agentd

import (
	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// The bridge/triage/errands runtimes were written against the runner's JSON
// config types (Allowlist, Locations, AgentConfig, Settings). agent.db is now
// the source of truth, so these adapters project the store into those types —
// the existing runtimes work unchanged, reading live data instead of JSON.

func (a *Agent) buildAllow() (runner.Allowlist, error) {
	entries, err := a.Store.ListAllow()
	if err != nil {
		return nil, err
	}
	out := runner.Allowlist{}
	for _, e := range entries {
		role := runner.Role(e.MaxRole)
		if role == "" {
			role = runner.RoleRead
		}
		out[e.Handle] = runner.AllowEntry{Role: role, Locations: []string{"*"}}
	}
	return out, nil
}

func (a *Agent) buildLocations() (runner.Locations, error) {
	wss, err := a.Store.ListWorkspaces(false)
	if err != nil {
		return nil, err
	}
	out := runner.Locations{}
	for _, w := range wss {
		name := w.Name
		if name == "" {
			name = w.Path
		}
		out[name] = runner.LocationConfig{Path: w.Path, MaxRole: runner.Role(w.MaxRole)}
	}
	return out, nil
}

func (a *Agent) buildAgents() (runner.AgentConfig, error) {
	cfg := runner.AgentConfig{Tasks: map[string]runner.Backend{}}
	personas, err := a.Store.ListPersonas()
	if err != nil {
		return cfg, err
	}
	for _, p := range personas {
		cfg.Tasks[p.Key] = runner.Backend(p.Backend)
	}
	return cfg, nil
}

func (a *Agent) buildSettings() (runner.Settings, error) {
	s := runner.Settings{}
	vals, err := a.Store.ListSettings()
	if err != nil {
		return s, err
	}
	s.MainUser = vals["main_user"]
	s.AutoReply = vals["auto_reply"]
	s.AutoReplyEnabled = vals["auto_reply_enabled"] == "true"
	s.Triage.Schedule = vals["triage_schedule"]
	s.Triage.Enabled = vals["triage_enabled"] == "true"
	// WhatsApp transport defaults: the socket path is deterministic (appdir/wa.sock,
	// matching comms); enable explicitly via the wa.enabled setting.
	dir, _ := runner.DefaultAppDir()
	s.WhatsApp.Socket = dir + "/wa.sock"
	s.WhatsApp.Enabled = vals["wa_enabled"] == "true"
	return s, nil
}

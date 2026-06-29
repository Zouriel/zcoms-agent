// Package bootstrap performs the agent's first-run setup: seed the default
// personas and import any legacy JSON config (locations/agents/allowlist/
// settings) into agent.db, then never again. It is the "JSON → store migration"
// step — after it runs, agent.db is the single source of truth and the old JSON
// files can be retired.
package bootstrap

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

const migratedKey = "migrated_json"

// Run seeds personas and imports legacy JSON once (guarded by a settings flag).
func Run(s *store.Store) error {
	if err := personas.SeedDefaults(s); err != nil {
		return err
	}
	// Migrate any superseded default seed (e.g. the Bridge row) to the current
	// default; edited rows are left untouched. Runs every start (idempotent).
	if err := personas.UpgradeDefaults(s); err != nil {
		return err
	}
	if done, _ := s.GetSetting(migratedKey); done == "1" {
		return nil
	}
	if err := importLegacyJSON(s); err != nil {
		return err
	}
	return s.SetSetting(store.Owner, migratedKey, "1")
}

// importLegacyJSON pulls the old hand-authored config into the store. Each source
// is best-effort: a missing/placeholder file just contributes nothing.
func importLegacyJSON(s *store.Store) error {
	dir, err := runner.DefaultAppDir()
	if err != nil {
		return err
	}

	// allowlist.json → allowlist (Telegram usernames + role cap).
	if exists(filepath.Join(dir, "agent-allowlist.json")) {
		if allow, _, err := runner.LoadOrSeedAllowlist(); err == nil {
			for handle, e := range allow {
				if handle == "" || strings.Contains(handle, "your_username") {
					continue
				}
				_, _ = s.CreateAllow(store.Owner, store.AllowEntry{
					Platform: "telegram", Handle: handle, MaxRole: string(e.Role),
				})
			}
		}
	}

	// locations.json → workspaces (imported as augmentation: path + name + cap).
	if exists(filepath.Join(dir, "agent-locations.json")) {
		if locs, _, err := runner.LoadOrSeedLocations(); err == nil {
			for name, cfg := range locs {
				if cfg.Path == "" {
					continue
				}
				if err := s.UpsertDiscovered(cfg.Path, name); err == nil {
					if w, ok, _ := s.GetWorkspaceByPath(cfg.Path); ok {
						nm := name
						var cap *string
						if string(cfg.MaxRole) != "" {
							c := string(cfg.MaxRole)
							cap = &c
						}
						_ = s.UpdateWorkspace(store.Owner, w.ID, &nm, cap, nil, nil)
					}
				}
			}
		}
	}

	// agents.json → persona backends (task → persona key, best-effort).
	if exists(filepath.Join(dir, "agents.json")) {
		if ac, _, err := runner.LoadOrSeedAgents(); err == nil {
			for task, backend := range ac.Tasks {
				key := task
				if task == "chat" {
					key = personas.Bridge
				}
				if p, ok, _ := s.GetPersona(key); ok {
					p.Backend = string(backend)
					_ = s.UpdatePersona(store.Owner, key, p)
				}
			}
		}
	}

	// agent-settings.json → settings (scalars the agent owns).
	if exists(filepath.Join(dir, "agent-settings.json")) {
		if set, _, err := runner.LoadOrSeedSettings(); err == nil {
			if set.MainUser != "" && !strings.Contains(set.MainUser, "your_username") {
				_ = s.SetSetting(store.Owner, "main_user", set.MainUser)
			}
			_ = s.SetSetting(store.Owner, "auto_reply_enabled", strconv.FormatBool(set.AutoReplyEnabled))
			if set.AutoReply != "" {
				_ = s.SetSetting(store.Owner, "auto_reply", set.AutoReply)
			}
			if set.Triage.Schedule != "" {
				_ = s.SetSetting(store.Owner, "triage_schedule", set.Triage.Schedule)
			}
			_ = s.SetSetting(store.Owner, "triage_enabled", strconv.FormatBool(set.Triage.Enabled))
		}
	}

	return nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

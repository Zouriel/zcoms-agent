package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// Aliases to the shared SDK types/funcs so the ported bridge code compiles
// against them with no edits.
type (
	Role           = runner.Role
	Backend        = runner.Backend
	AllowEntry     = runner.AllowEntry
	Allowlist      = runner.Allowlist
	Locations      = runner.Locations
	LocationConfig = runner.LocationConfig
	Session        = runner.Session
	RunResult      = runner.RunResult
	AgentConfig    = runner.AgentConfig
	Settings       = runner.Settings
	TriageSettings = runner.TriageSettings
)

const (
	RoleRead    = runner.RoleRead
	RoleConfirm = runner.RoleConfirm
	RoleEdit    = runner.RoleEdit
	RoleFull    = runner.RoleFull

	BackendClaude = runner.BackendClaude
	BackendCodex  = runner.BackendCodex
)

var (
	ListSessionsFor     = runner.ListSessionsFor
	RunAgent            = runner.RunAgent
	MinRole             = runner.MinRole
	ValidRole           = runner.ValidRole
	LoadOrSeedLocations = runner.LoadOrSeedLocations
	LoadOrSeedSettings  = runner.LoadOrSeedSettings
)

// roleRank mirrors the SDK's unexported Role.rank (read<confirm<edit<full), used
// to cap a user's role by a location ceiling.
func roleRank(r Role) int {
	switch r {
	case RoleRead:
		return 1
	case RoleConfirm:
		return 2
	case RoleEdit:
		return 3
	case RoleFull:
		return 4
	}
	return 0
}

func platformLabel(source string) string {
	if source == "wa" {
		return "WhatsApp"
	}
	return "Telegram"
}

func snippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// Recipient/TriageBatch mirror the triage component's last-triage.json so
// `interact triage` can reply to whoever wrote in.
type Recipient struct {
	Index    int      `json:"index"`
	Source   string   `json:"source"`
	Name     string   `json:"name"`
	TGChat   int64    `json:"tg_chat,omitempty"`
	WAChat   string   `json:"wa_chat,omitempty"`
	Messages []string `json:"messages"`
	Files    []string `json:"files,omitempty"`
}

type TriageBatch struct {
	At         time.Time   `json:"at"`
	Recipients []Recipient `json:"recipients"`
}

func configDir() (string, error) { return runner.DefaultAppDir() }

func LoadTriageBatch() (TriageBatch, error) {
	dir, err := configDir()
	if err != nil {
		return TriageBatch{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "last-triage.json"))
	if os.IsNotExist(err) {
		return TriageBatch{}, nil
	}
	if err != nil {
		return TriageBatch{}, err
	}
	var b TriageBatch
	if err := json.Unmarshal(data, &b); err != nil {
		return TriageBatch{}, err
	}
	return b, nil
}

func ensureStagingDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "agent-staging")
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

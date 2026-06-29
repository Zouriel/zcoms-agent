// Package agent is the shared zcoms toolkit used by the core daemon and every
// component (bridge/triage/errands): config readers for the agent JSON files,
// agent-backend selection, session listing, and the claude/codex runner. It is
// pure Go (no TDLib/cgo) so components build and run as plain IPC clients.
package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Role controls how much an allow-listed user can make Claude do.
type Role string

const (
	RoleFull    Role = "full"    // can do anything (--dangerously-skip-permissions)
	RoleEdit    Role = "edit"    // read/write/run, auto-approved (acceptEdits)
	RoleConfirm Role = "confirm" // plans first, executes only after you approve in Telegram
	RoleRead    Role = "read"    // inspect/plan only, never acts (plan mode)
)

// rank orders roles from least to most powerful, for capping.
func (r Role) rank() int {
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

func (r Role) valid() bool { return r.rank() > 0 }

// ValidRole reports whether r is a known role (read|confirm|edit|full).
func ValidRole(r Role) bool { return r.valid() }

// MinRole returns the more restrictive (less powerful) of two roles.
func MinRole(a, b Role) Role {
	if a.rank() <= b.rank() {
		return a
	}
	return b
}

// AllowEntry is one allow-listed user's permissions.
type AllowEntry struct {
	Role      Role     `json:"role"`
	Locations []string `json:"locations"`       // location names, or ["*"] for all
	Agent     Backend  `json:"agent,omitempty"` // "claude" (default) | "codex"

	UserID int64 `json:"-"` // resolved from the @username at startup
}

// AllowsLocation reports whether this entry may use the named location.
func (e AllowEntry) AllowsLocation(name string) bool {
	for _, l := range e.Locations {
		if l == "*" || l == name {
			return true
		}
	}
	return false
}

// Allowlist maps @username -> permissions.
type Allowlist map[string]AllowEntry

// LocationConfig is a project directory plus an optional ceiling on what any
// user may do there (e.g. a production repo capped to "read" regardless of role).
type LocationConfig struct {
	Path    string `json:"path"`
	MaxRole Role   `json:"max_role,omitempty"`
}

// Locations maps a friendly name -> location config. In JSON each value may be
// either a plain path string or an object {"path": ..., "max_role": ...}.
type Locations map[string]LocationConfig

// MarshalJSON writes a plain path string when there's no cap, else an object,
// keeping agent-locations.json tidy.
func (l Locations) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	for name, cfg := range l {
		if cfg.MaxRole == "" {
			out[name] = cfg.Path
		} else {
			out[name] = cfg
		}
	}
	return json.Marshal(out)
}

func (l *Locations) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := Locations{}
	for name, value := range raw {
		var asString string
		if json.Unmarshal(value, &asString) == nil {
			out[name] = LocationConfig{Path: asString}
			continue
		}
		var cfg LocationConfig
		if err := json.Unmarshal(value, &cfg); err != nil {
			return err
		}
		out[name] = cfg
	}
	*l = out
	return nil
}

const (
	locationsFile = "agent-locations.json"
	allowlistFile = "agent-allowlist.json"
)

func configDir() (string, error) {
	return DefaultAppDir()
}

// stagingDirName is the per-agent scratch dir the sandboxed triage/chat agent
// can write to (e.g. to produce a screenshot) before SENDFILE-ing it.
const stagingDirName = "agent-staging"

// ensureStagingDir returns (creating if needed) the writable scratch dir handed
// to interactive triage/chat agents as their working directory.
func ensureStagingDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, stagingDirName)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// LoadOrSeedLocations reads agent-locations.json, creating a placeholder file on
// first run so the user has something to edit.
func LoadOrSeedLocations() (Locations, string, error) {
	path, _ := configFilePath()
	var locations Locations
	found, err := loadSection("locations", &locations)
	if err != nil {
		return nil, path, err
	}
	if !found {
		locations = Locations{
			"example":       {Path: "/absolute/path/to/a/project"},
			"prod-readonly": {Path: "/absolute/path/to/prod", MaxRole: RoleRead},
		}
		_ = saveSection("locations", locations)
	}
	return locations, path, nil
}

// LoadOrSeedAllowlist reads the allowlist section, creating a placeholder on
// first run. Entries with an invalid/empty role are dropped.
func LoadOrSeedAllowlist() (Allowlist, string, error) {
	path, _ := configFilePath()
	var raw Allowlist
	found, err := loadSection("allowlist", &raw)
	if err != nil {
		return nil, path, err
	}
	if !found {
		seed := Allowlist{"@your_username": {Role: RoleFull, Locations: []string{"*"}}}
		_ = saveSection("allowlist", seed)
		return seed, path, nil
	}
	cleaned := Allowlist{}
	for username, entry := range raw {
		if !entry.Role.valid() {
			continue
		}
		if len(entry.Locations) == 0 {
			entry.Locations = []string{"*"}
		}
		cleaned[username] = entry
	}
	return cleaned, path, nil
}

// SaveLocations writes the locations section of config.json.
func SaveLocations(l Locations) (string, error) {
	path, _ := configFilePath()
	return path, saveSection("locations", l)
}

// SaveAllowlist writes the allowlist section of config.json.
func SaveAllowlist(a Allowlist) (string, error) {
	path, _ := configFilePath()
	return path, saveSection("allowlist", a)
}

// SortedLocationNames returns location names in stable alphabetical order so the
// numbered menu is consistent between calls.
func (l Locations) SortedNames() []string {
	names := make([]string, 0, len(l))
	for name := range l {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// --- allow-list key normalization (shared by the agent's buildAllow and the
// bridge's lookupAllow so a stored entry and an inbound sender map identically) ---

// WADigits reduces a WhatsApp number/jid to its bare digits: it drops a jid
// suffix ("…@s.whatsapp.net"), "+", spaces and punctuation. "+960 765-4321" and
// "9607654321@s.whatsapp.net" both become "9607654321".
func WADigits(s string) string {
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// AllowKey is the canonical "<platform>|<handle>" map key. Telegram handles are
// lower-cased and @-prefixed (usernames are case-insensitive); WhatsApp handles
// reduce to digits. Both the stored allow-list and an inbound sender go through
// here, so matching is platform-aware and format-insensitive.
func AllowKey(platform, handle string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "whatsapp":
		return "whatsapp|" + WADigits(handle)
	default:
		h := strings.ToLower(strings.TrimSpace(handle))
		if h != "" && !strings.HasPrefix(h, "@") && !looksLikePhone(h) {
			h = "@" + h
		}
		return "telegram|" + h
	}
}

// NormalizeAllowHandle tidies a handle for storage/display: a Telegram username
// gets its leading @ (a phone is left alone); a WhatsApp number reduces to digits.
func NormalizeAllowHandle(platform, handle string) string {
	if strings.EqualFold(strings.TrimSpace(platform), "whatsapp") {
		return WADigits(handle)
	}
	h := strings.TrimSpace(handle)
	if h != "" && !strings.HasPrefix(h, "@") && !looksLikePhone(h) {
		h = "@" + h
	}
	return h
}

func looksLikePhone(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s[0] == '+' {
		return true
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

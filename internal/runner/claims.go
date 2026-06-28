package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// claims.json lists the chats currently "owned" by a component (today: errands),
// so the daemon's `unread` op and the triage component skip them, and the daemon
// routes their incoming messages to the owning component. The owning component
// is the sole writer; the daemon and triage are readers.
const claimsFile = "claims.json"

// Claims is the set of chats currently owned by a component.
type Claims struct {
	TG []int64  `json:"tg"`
	WA []string `json:"wa"`
}

// HasTG reports whether a Telegram chat is claimed.
func (c Claims) HasTG(id int64) bool {
	for _, x := range c.TG {
		if x == id {
			return true
		}
	}
	return false
}

// HasWA reports whether a WhatsApp chat is claimed.
func (c Claims) HasWA(id string) bool {
	for _, x := range c.WA {
		if x == id {
			return true
		}
	}
	return false
}

func claimsPath() (string, error) {
	dir, err := DefaultAppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, claimsFile), nil
}

// LoadClaims reads claims.json (missing file => empty, not an error).
func LoadClaims() (Claims, error) {
	p, err := claimsPath()
	if err != nil {
		return Claims{}, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return Claims{}, nil
	}
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	if err := json.Unmarshal(data, &c); err != nil {
		return Claims{}, err
	}
	return c, nil
}

// SaveClaims writes claims.json atomically (temp + rename) so readers never see
// a half-written file.
func SaveClaims(c Claims) error {
	p, err := claimsPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

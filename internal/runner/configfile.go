package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// Unified config: a single ~/.config/zcoms/config.json with one top-level
// section per concern (core, components, settings, agents, allowlist,
// locations, …). It grows a section as modules are installed. On first read it
// migrates the legacy split files into sections (idempotent), guarded by a
// cross-process flock so the daemon and components never corrupt it.

const configFileName = "config.json"

// legacySections maps a unified section key to the old standalone file.
var legacySections = map[string]string{
	"settings":   "agent-settings.json",
	"agents":     "agents.json",
	"allowlist":  "agent-allowlist.json",
	"locations":  "agent-locations.json",
	"components": "components.json",
}

func configFilePath() (string, error) {
	dir, err := DefaultAppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

func lockConfig() func() {
	dir, err := DefaultAppDir()
	if err != nil {
		return func() {}
	}
	f, err := os.OpenFile(filepath.Join(dir, "config.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX) != nil {
		f.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

func readRawConfig() map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	p, err := configFilePath()
	if err != nil {
		return m
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func writeRawConfig(m map[string]json.RawMessage) error {
	p, err := configFilePath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ensureConfigMigrated folds the legacy split files into the unified config.json
// once. Detection: the legacy core config.json has "tdlib_dir" at top level; the
// unified file never does.
func ensureConfigMigrated() {
	unlock := lockConfig()
	defer unlock()
	raw := readRawConfig()
	if _, legacy := raw["tdlib_dir"]; !legacy {
		return // already unified (or empty/fresh)
	}
	unified := map[string]json.RawMessage{}
	if b, err := json.Marshal(raw); err == nil {
		unified["core"] = b // the legacy config.json content becomes the core section
	}
	dir, _ := DefaultAppDir()
	for key, fname := range legacySections {
		if data, err := os.ReadFile(filepath.Join(dir, fname)); err == nil {
			unified[key] = json.RawMessage(data)
		}
	}
	_ = writeRawConfig(unified)
}

// loadSection unmarshals one section into dest; found is false if absent.
func loadSection(key string, dest any) (found bool, err error) {
	ensureConfigMigrated()
	raw := readRawConfig()
	b, ok := raw[key]
	if !ok || len(b) == 0 {
		return false, nil
	}
	return true, json.Unmarshal(b, dest)
}

// saveSection writes one section, preserving the rest of the file.
func saveSection(key string, value any) error {
	ensureConfigMigrated()
	unlock := lockConfig()
	defer unlock()
	raw := readRawConfig()
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw[key] = b
	return writeRawConfig(raw)
}

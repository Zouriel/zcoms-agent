//go:build windows

package bridge

import (
	"os"
	"path/filepath"
	"time"
)

// lockTriageBrain serializes the shared triage session on Windows (no flock) via
// an exclusive-create lock file with brief retry. Returns nil on failure (fail-open).
func lockTriageBrain() func() {
	dir, err := configDir()
	if err != nil {
		return nil
	}
	lock := filepath.Join(dir, "triage-brain.lock")
	for i := 0; i < 250; i++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return func() { _ = f.Close(); _ = os.Remove(lock) }
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

//go:build !windows

package bridge

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockTriageBrain takes a blocking cross-process flock on the triage brain (the
// same lockfile the triage loop uses), serializing the shared session. Returns
// an unlock func, or nil on error (fail-open).
func lockTriageBrain() func() {
	dir, err := configDir()
	if err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "triage-brain.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

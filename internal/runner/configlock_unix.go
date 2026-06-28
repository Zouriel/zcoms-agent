//go:build !windows

package runner

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockConfig takes a cross-process advisory lock on config.lock (flock) so the
// daemon and components never corrupt the unified config.json with concurrent
// writes. Returns an unlock func (a no-op on any error — locking is best-effort).
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

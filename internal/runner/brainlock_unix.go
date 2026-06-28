//go:build !windows

package runner

import (
	"os"
	"path/filepath"
	"syscall"
)

const triageBrainLockFile = "triage-brain.lock"

// TryLockTriageBrain takes a non-blocking, cross-process lock on the triage
// brain session (an flock on a lockfile in the config dir, shared with the core
// daemon's `interact triage`). It returns an unlock func and whether the lock
// was acquired; callers that don't get it should skip this pass. Best-effort: on
// any error it reports acquired (fail-open) so triage never wedges.
func TryLockTriageBrain() (unlock func(), ok bool) {
	dir, err := DefaultAppDir()
	if err != nil {
		return func() {}, true
	}
	f, err := os.OpenFile(filepath.Join(dir, triageBrainLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, true
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return func() {}, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

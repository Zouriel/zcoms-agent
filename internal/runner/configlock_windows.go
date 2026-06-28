//go:build windows

package runner

import (
	"os"
	"path/filepath"
	"time"
)

// lockConfig serializes config writes on Windows (which lacks flock) via an
// exclusive-create lock file, retrying briefly. Best-effort, like the Unix path.
func lockConfig() func() {
	dir, err := DefaultAppDir()
	if err != nil {
		return func() {}
	}
	lock := filepath.Join(dir, "config.lock")
	for i := 0; i < 100; i++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return func() { _ = f.Close(); _ = os.Remove(lock) }
		}
		time.Sleep(20 * time.Millisecond)
	}
	return func() {}
}

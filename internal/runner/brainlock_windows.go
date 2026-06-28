//go:build windows

package runner

// TryLockTriageBrain is a no-op on Windows (the daemon and components run on
// Linux); it always reports the lock acquired.
func TryLockTriageBrain() (unlock func(), ok bool) {
	return func() {}, true
}

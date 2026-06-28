package runner

import "github.com/Zouriel/zcoms/client"

// DefaultAppDir is the zcoms config/state directory (~/.config/zcoms), resolved
// through the single canonical resolver in comms/client so every tier agrees.
func DefaultAppDir() (string, error) {
	return client.DefaultAppDir()
}

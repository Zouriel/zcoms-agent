package agentd

import (
	"log"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// runMigrations applies the one-time data clean-ups that follow a removed
// feature. Each is idempotent, so running them on every start is harmless; a
// failure is logged and never stops the agent.
func runMigrations(st *store.Store, lg *log.Logger) {
	n, err := st.PurgeRetiredTransports()
	if err != nil {
		lg.Printf("migration: retired transports: %v", err)
	} else if n > 0 {
		lg.Printf("migration: removed %d allowlist/triage row(s) for instagram, discord or viber", n)
	}
}

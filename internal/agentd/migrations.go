package agentd

import (
	"log"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// retiredStandupPersona is the persona key of the removed team standup
// interviewer. Its row is deleted once on start.
const retiredStandupPersona = "standup_interviewer"

// runMigrations applies the one-time data clean-ups that follow a removed
// feature. Each is idempotent, so running them on every start is harmless; a
// failure is logged and never stops the agent.
func runMigrations(st *store.Store, lg *log.Logger) {
	if ok, err := st.DeletePersonaIfPresent(retiredStandupPersona); err != nil {
		lg.Printf("migration: retired persona: %v", err)
	} else if ok {
		lg.Printf("migration: removed the %s persona (team standups are gone)", retiredStandupPersona)
	}
	n, err := st.PurgeRetiredTransports()
	if err != nil {
		lg.Printf("migration: retired transports: %v", err)
	} else if n > 0 {
		lg.Printf("migration: removed %d allowlist/triage row(s) for instagram, discord or viber", n)
	}
}

package store

import "fmt"

// Session decorates a backend session anchored to a workspace. Existence is
// enumerated LIVE from the backend (claude/codex) — this table only adds a label
// and a last-resumed stamp. There is no Create/Delete API: the store never
// claims to know which sessions exist.
type Session struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspace_id"`
	Backend     string `json:"backend"`
	ExternalID  string `json:"external_id"`
	Label       string `json:"label"`
}

// ListSessionDecorations returns the stored decorations for a workspace, keyed
// by external_id, so a live enumeration can join labels onto real sessions.
func (s *Store) ListSessionDecorations(workspaceID int64) (map[string]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_id, backend, external_id, COALESCE(label,'') FROM sessions WHERE workspace_id=?`,
		workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Session{}
	for rows.Next() {
		var d Session
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Backend, &d.ExternalID, &d.Label); err != nil {
			return nil, err
		}
		out[d.ExternalID] = d
	}
	return out, rows.Err()
}

// SetLabel sets/updates the decoration for a (backend, external_id) session,
// creating the decoration row on first label. This is NOT a session "create" —
// it only attaches a label to a session the backend already owns. Agent-allowed.
func (s *Store) SetLabel(_ Caller, workspaceID int64, backend, externalID, label string) error {
	if err := validateBackend(backend); err != nil {
		return err
	}
	// Upsert the decoration; the UNIQUE(backend, external_id) makes this idempotent.
	_, err := s.db.Exec(`
		INSERT INTO sessions(workspace_id, backend, external_id, label, last_resumed_at, created_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(backend, external_id) DO UPDATE SET label=excluded.label, last_resumed_at=excluded.last_resumed_at`,
		workspaceID, backend, externalID, label, now(), now())
	return err
}

// TouchResumed stamps a session as just-resumed (decoration only).
func (s *Store) TouchResumed(workspaceID int64, backend, externalID string) error {
	if err := validateBackend(backend); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO sessions(workspace_id, backend, external_id, last_resumed_at, created_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(backend, external_id) DO UPDATE SET last_resumed_at=excluded.last_resumed_at`,
		workspaceID, backend, externalID, now(), now())
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

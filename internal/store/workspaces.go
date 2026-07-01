package store

import (
	"database/sql"
	"fmt"
)

// Workspace is a named, permission-capped directory an agent may run in. The
// path is ground truth (discovered on disk); the other columns are augmentation
// the owner adds on top. There is deliberately NO Create/Delete API — discovery
// owns existence; "remove" is Ignore (the sync then skips it).
type Workspace struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	MaxRole    string `json:"max_role"`
	Discovered bool   `json:"discovered"`
	Present    bool   `json:"present"`
	Ignored    bool   `json:"ignored"`
	Pinned     bool   `json:"pinned"`
}

func scanWorkspace(rows *sql.Rows) (Workspace, error) {
	var w Workspace
	var name, maxRole sql.NullString
	var disc, pres, ign, pin int
	err := rows.Scan(&w.ID, &w.Path, &name, &maxRole, &disc, &pres, &ign, &pin)
	w.Name, w.MaxRole = name.String, maxRole.String
	w.Discovered, w.Present, w.Ignored, w.Pinned = disc == 1, pres == 1, ign == 1, pin == 1
	return w, err
}

const wsCols = `id, path, name, max_role, discovered, present, ignored, pinned`

// ListWorkspaces returns workspaces (pinned first, then name/path), optionally
// including ignored/absent ones.
func (s *Store) ListWorkspaces(includeHidden bool) ([]Workspace, error) {
	q := `SELECT ` + wsCols + ` FROM workspaces`
	if !includeHidden {
		q += ` WHERE ignored=0 AND present=1`
	}
	q += ` ORDER BY pinned DESC, COALESCE(name, path)`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWorkspace looks a workspace up by id.
func (s *Store) GetWorkspace(id int64) (Workspace, error) {
	rows, err := s.db.Query(`SELECT `+wsCols+` FROM workspaces WHERE id=?`, id)
	if err != nil {
		return Workspace{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Workspace{}, fmt.Errorf("no workspace with id %d", id)
	}
	return scanWorkspace(rows)
}

// GetWorkspaceByPath looks a workspace up by its (unique) path.
func (s *Store) GetWorkspaceByPath(path string) (Workspace, bool, error) {
	rows, err := s.db.Query(`SELECT `+wsCols+` FROM workspaces WHERE path=?`, path)
	if err != nil {
		return Workspace{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Workspace{}, false, nil
	}
	w, err := scanWorkspace(rows)
	return w, true, err
}

// UpdateWorkspace changes the augmentation columns. name/pinned/ignored are
// agent-writable; max_role is owner-only (it is a permission cap — a
// prompt-injected agent must not be able to widen its own reach). Passing a nil
// pointer leaves that column unchanged.
func (s *Store) UpdateWorkspace(c Caller, id int64, name, maxRole *string, pinned, ignored *bool) error {
	if maxRole != nil {
		if err := requireOwner(c, "workspace max_role"); err != nil {
			return err
		}
		if err := validateRole(*maxRole); err != nil {
			return err
		}
	}
	sets := []string{}
	args := []any{}
	if name != nil {
		sets = append(sets, "name=?")
		args = append(args, *name)
	}
	if maxRole != nil {
		sets = append(sets, "max_role=?")
		args = append(args, *maxRole)
	}
	if pinned != nil {
		sets = append(sets, "pinned=?")
		args = append(args, boolToInt(*pinned))
	}
	if ignored != nil {
		sets = append(sets, "ignored=?")
		args = append(args, boolToInt(*ignored))
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at=?")
	args = append(args, now(), id)
	res, err := s.db.Exec(`UPDATE workspaces SET `+join(sets)+` WHERE id=?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no workspace with id %d", id)
	}
	return nil
}

// SetIgnored is the "remove" for a workspace: discovery-sync skips ignored paths
// (never a hard delete, so the cap/history survives a return). Agent-allowed.
func (s *Store) SetIgnored(c Caller, id int64, ignored bool) error {
	return s.UpdateWorkspace(c, id, nil, nil, nil, &ignored)
}

// UpsertDiscovered is the discovery-sync write path (NOT exposed to callers as
// create/delete): it inserts a freshly-found repo or marks an existing one
// present again, preserving all augmentation. It never runs as the agent guard —
// it is the registry's own ground-truth reconciliation.
func (s *Store) UpsertDiscovered(path, derivedName string) error {
	existing, ok, err := s.GetWorkspaceByPath(path)
	if err != nil {
		return err
	}
	if ok {
		_, err = s.db.Exec(`UPDATE workspaces SET present=1, updated_at=? WHERE id=?`, now(), existing.ID)
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO workspaces(path, name, discovered, present, created_at, updated_at) VALUES(?,?,1,1,?,?)`,
		path, derivedName, now(), now(),
	)
	return err
}

// MarkAbsent flags every workspace whose path is not in present as present=0
// (vanished from disk) without hard-deleting it.
func (s *Store) MarkAbsent(presentPaths map[string]bool) error {
	rows, err := s.db.Query(`SELECT id, path FROM workspaces WHERE present=1`)
	if err != nil {
		return err
	}
	var goneIDs []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if !presentPaths[p] {
			goneIDs = append(goneIDs, id)
		}
	}
	rows.Close()
	for _, id := range goneIDs {
		if _, err := s.db.Exec(`UPDATE workspaces SET present=0, updated_at=? WHERE id=?`, now(), id); err != nil {
			return err
		}
	}
	return nil
}

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

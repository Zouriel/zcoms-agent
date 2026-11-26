package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Group kinds. A group with no policy row is off.
const (
	GroupGeneral = "general"
	GroupProject = "project"
)

// GroupPolicy says what the agent may do in one group. Only owner-caller
// commands write it; a group call, a repo file or a model turn never can.
type GroupPolicy struct {
	ID        int64  `json:"id"`
	Transport string `json:"transport"`
	NativeID  string `json:"native_id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ProjectID int64  `json:"project_id,omitempty"`
	LastCall  int64  `json:"last_call,omitempty"` // unix seconds
}

// Project binds one folder (a workspace row) to one group. The binding is
// authoritative here; .zcoms in the folder is display only.
type Project struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspace_id"`
	Path        string `json:"path"`
	MaxRole     string `json:"max_role,omitempty"`
	Transport   string `json:"transport"`
	NativeID    string `json:"native_id"`
	GroupName   string `json:"group_name"`
	FollowTasks bool   `json:"follow_tasks"`
}

const groupsSchema = `
CREATE TABLE IF NOT EXISTS group_policy (
  id INTEGER PRIMARY KEY,
  transport TEXT NOT NULL,
  native_id TEXT NOT NULL,
  name TEXT,
  kind TEXT NOT NULL,
  project_id INTEGER,
  last_call_at INTEGER,
  created_at TEXT, updated_at TEXT,
  UNIQUE(transport, native_id)
);
CREATE TABLE IF NOT EXISTS projects (
  id INTEGER PRIMARY KEY,
  workspace_id INTEGER NOT NULL UNIQUE REFERENCES workspaces(id),
  transport TEXT NOT NULL,
  native_id TEXT NOT NULL,
  group_name TEXT,
  follow_tasks INTEGER NOT NULL DEFAULT 0,
  created_at TEXT, updated_at TEXT
);`

func validKind(k string) bool { return k == GroupGeneral || k == GroupProject }

// GetGroupPolicy returns the policy row for a group, ok=false when the group
// is off.
func (s *Store) GetGroupPolicy(transport, nativeID string) (GroupPolicy, bool, error) {
	var p GroupPolicy
	var name sql.NullString
	var proj, last sql.NullInt64
	err := s.db.QueryRow(`SELECT id, transport, native_id, name, kind, project_id, last_call_at
		FROM group_policy WHERE transport=? AND native_id=?`, transport, nativeID).
		Scan(&p.ID, &p.Transport, &p.NativeID, &name, &p.Kind, &proj, &last)
	if err == sql.ErrNoRows {
		return GroupPolicy{}, false, nil
	}
	if err != nil {
		return GroupPolicy{}, false, err
	}
	p.Name, p.ProjectID, p.LastCall = name.String, proj.Int64, last.Int64
	return p, true, nil
}

// ListGroupPolicies returns every group the agent answers in.
func (s *Store) ListGroupPolicies() ([]GroupPolicy, error) {
	rows, err := s.db.Query(`SELECT id, transport, native_id, name, kind, project_id, last_call_at
		FROM group_policy ORDER BY transport, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupPolicy
	for rows.Next() {
		var p GroupPolicy
		var name sql.NullString
		var proj, last sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Transport, &p.NativeID, &name, &p.Kind, &proj, &last); err != nil {
			return nil, err
		}
		p.Name, p.ProjectID, p.LastCall = name.String, proj.Int64, last.Int64
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetGroupPolicy creates or replaces a group's policy. Owner-only.
func (s *Store) SetGroupPolicy(c Caller, p GroupPolicy) error {
	if err := requireOwner(c, "group_policy"); err != nil {
		return err
	}
	if !validKind(p.Kind) {
		return fmt.Errorf("invalid group kind %q (want general|project)", p.Kind)
	}
	if p.Transport == "" || p.NativeID == "" {
		return fmt.Errorf("group policy needs a transport and a group id")
	}
	var proj any
	if p.ProjectID != 0 {
		proj = p.ProjectID
	}
	_, err := s.db.Exec(`INSERT INTO group_policy(transport, native_id, name, kind, project_id, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(transport, native_id) DO UPDATE SET name=excluded.name, kind=excluded.kind,
		  project_id=excluded.project_id, updated_at=excluded.updated_at`,
		p.Transport, p.NativeID, p.Name, p.Kind, proj, now(), now())
	return err
}

// DeleteGroupPolicy turns a group off. Owner-only. Deleting a missing row is
// not an error.
func (s *Store) DeleteGroupPolicy(c Caller, transport, nativeID string) error {
	if err := requireOwner(c, "group_policy"); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM group_policy WHERE transport=? AND native_id=?`, transport, nativeID)
	return err
}

// TouchGroupCall records when a group last called the agent. It is the
// agent's own bookkeeping (shown in the console), so any caller may write it,
// and it only updates an existing row.
func (s *Store) TouchGroupCall(transport, nativeID string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE group_policy SET last_call_at=? WHERE transport=? AND native_id=?`,
		at.Unix(), transport, nativeID)
	return err
}

const projectCols = `p.id, p.workspace_id, w.path, w.max_role, p.transport, p.native_id, p.group_name, p.follow_tasks`

func scanProject(sc interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var role, name sql.NullString
	var follow int
	err := sc.Scan(&p.ID, &p.WorkspaceID, &p.Path, &role, &p.Transport, &p.NativeID, &name, &follow)
	p.MaxRole, p.GroupName, p.FollowTasks = role.String, name.String, follow == 1
	return p, err
}

// ListProjects returns every bound project with its folder path.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM projects p JOIN workspaces w ON w.id=p.workspace_id ORDER BY w.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject returns one project by id.
func (s *Store) GetProject(id int64) (Project, bool, error) {
	p, err := scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE p.id=?`, id))
	if err == sql.ErrNoRows {
		return Project{}, false, nil
	}
	return p, err == nil, err
}

// ProjectForPath returns the project whose folder is path or encloses it (the
// deepest match wins), so a command run anywhere inside a bound folder finds
// its binding. Matching is by absolute path: another clone is not the project.
func (s *Store) ProjectForPath(path string) (Project, bool, error) {
	path = filepath.Clean(path)
	all, err := s.ListProjects()
	if err != nil {
		return Project{}, false, err
	}
	var best Project
	found := false
	for _, p := range all {
		root := filepath.Clean(p.Path)
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			if !found || len(root) > len(best.Path) {
				best, found = p, true
			}
		}
	}
	return best, found, nil
}

// CreateProject binds a workspace to a group and writes the matching project
// group policy in one transaction. Owner-only.
func (s *Store) CreateProject(c Caller, p Project) (Project, error) {
	if err := requireOwner(c, "projects"); err != nil {
		return Project{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO projects(workspace_id, transport, native_id, group_name, follow_tasks, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)`, p.WorkspaceID, p.Transport, p.NativeID, p.GroupName, boolToInt(p.FollowTasks), now(), now())
	if err != nil {
		return Project{}, fmt.Errorf("binding project: %w", err)
	}
	p.ID, _ = res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO group_policy(transport, native_id, name, kind, project_id, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(transport, native_id) DO UPDATE SET name=excluded.name, kind=excluded.kind,
		  project_id=excluded.project_id, updated_at=excluded.updated_at`,
		p.Transport, p.NativeID, p.GroupName, GroupProject, p.ID, now(), now()); err != nil {
		return Project{}, err
	}
	return p, tx.Commit()
}

// DeleteProject unbinds a project and turns its group off. Owner-only.
func (s *Store) DeleteProject(c Caller, id int64) error {
	if err := requireOwner(c, "projects"); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM group_policy WHERE kind=? AND project_id=?`, GroupProject, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetProjectFollow turns a project's follow tasks on or off. Owner-only.
func (s *Store) SetProjectFollow(c Caller, id int64, on bool) error {
	if err := requireOwner(c, "projects"); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE projects SET follow_tasks=?, updated_at=? WHERE id=?`, boolToInt(on), now(), id)
	return err
}

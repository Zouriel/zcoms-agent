package store

import (
	"fmt"
	"strings"
)

// AllowEntry is one principal permitted to drive the agent, with a role cap.
// The allowlist is enforced HERE (agent tier), not in the comms daemon. It is a
// crown jewel — owner-only writes, so a prompt-injected errand cannot add itself.
type AllowEntry struct {
	ID       int64  `json:"id"`
	Platform string `json:"platform"`
	Handle   string `json:"handle"`
	MaxRole  string `json:"max_role"`
}

// ListAllow returns the allowlist.
func (s *Store) ListAllow() ([]AllowEntry, error) {
	rows, err := s.db.Query(`SELECT id, platform, handle, COALESCE(max_role,'') FROM allowlist ORDER BY platform, handle`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AllowEntry
	for rows.Next() {
		var e AllowEntry
		if err := rows.Scan(&e.ID, &e.Platform, &e.Handle, &e.MaxRole); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Allowed reports whether (platform, handle) may drive the agent, and its cap.
func (s *Store) Allowed(platform, handle string) (AllowEntry, bool, error) {
	var e AllowEntry
	var role string
	err := s.db.QueryRow(`SELECT id, platform, handle, COALESCE(max_role,'') FROM allowlist WHERE platform=? AND handle=?`,
		strings.ToLower(platform), handle).Scan(&e.ID, &e.Platform, &e.Handle, &role)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return AllowEntry{}, false, nil
		}
		return AllowEntry{}, false, err
	}
	e.MaxRole = role
	return e, true, nil
}

// CreateAllow adds an allowlist entry. Owner-only.
func (s *Store) CreateAllow(c Caller, e AllowEntry) (AllowEntry, error) {
	if err := requireOwner(c, "allowlist"); err != nil {
		return e, err
	}
	if strings.TrimSpace(e.Platform) == "" || strings.TrimSpace(e.Handle) == "" {
		return e, fmt.Errorf("an allowlist entry needs a platform and handle")
	}
	if err := validateRole(e.MaxRole); err != nil {
		return e, err
	}
	res, err := s.db.Exec(`INSERT INTO allowlist(platform, handle, max_role) VALUES(?,?,?)`,
		strings.ToLower(e.Platform), e.Handle, e.MaxRole)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return e, fmt.Errorf("%s %s is already allow-listed", e.Platform, e.Handle)
		}
		return e, err
	}
	e.ID, _ = res.LastInsertId()
	return e, nil
}

// UpdateAllow changes an entry's max_role. Owner-only.
func (s *Store) UpdateAllow(c Caller, id int64, maxRole string) error {
	if err := requireOwner(c, "allowlist"); err != nil {
		return err
	}
	if err := validateRole(maxRole); err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE allowlist SET max_role=? WHERE id=?`, maxRole, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no allowlist entry with id %d", id)
	}
	return nil
}

// DeleteAllow removes an entry. Owner-only.
func (s *Store) DeleteAllow(c Caller, id int64) error {
	if err := requireOwner(c, "allowlist"); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM allowlist WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no allowlist entry with id %d", id)
	}
	return nil
}

package store

import (
	"fmt"
	"strings"
)

// TriageGroup is a named set of sources triaged together on its own schedule.
// Groups let the owner batch apps (e.g. "work apps @ 09:00") or split them
// ("personal @ hourly"). Like the allowlist, groups are a crown jewel —
// owner-only writes.
type TriageGroup struct {
	ID           int64          `json:"id"`
	Name         string         `json:"name"`
	ScheduleKind string         `json:"schedule_kind"` // "interval" | "daily"
	ScheduleSpec string         `json:"schedule_spec"` // "1h" | "09:00,18:00"
	Enabled      bool           `json:"enabled"`
	LastRunAt    string         `json:"last_run_at,omitempty"`
	Sources      []TriageSource `json:"sources"`
}

// TriageSource is one (transport, account, chat filter) a group triages.
type TriageSource struct {
	ID         int64  `json:"id,omitempty"`
	GroupID    int64  `json:"group_id,omitempty"`
	Transport  string `json:"transport"`             // "telegram"|"whatsapp"|"instagram"
	Account    string `json:"account,omitempty"`     // which connected account (if >1)
	ChatFilter string `json:"chat_filter,omitempty"` // optional include/exclude
}

var validScheduleKinds = map[string]bool{"interval": true, "daily": true}

// ListTriageGroups returns every group with its sources, ordered by name.
func (s *Store) ListTriageGroups() ([]TriageGroup, error) {
	rows, err := s.db.Query(`SELECT id, name, schedule_kind, schedule_spec, enabled, COALESCE(last_run_at,'') FROM triage_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriageGroup
	byID := map[int64]int{}
	for rows.Next() {
		var g TriageGroup
		var enabled int
		if err := rows.Scan(&g.ID, &g.Name, &g.ScheduleKind, &g.ScheduleSpec, &enabled, &g.LastRunAt); err != nil {
			return nil, err
		}
		g.Enabled = enabled != 0
		g.Sources = []TriageSource{}
		byID[g.ID] = len(out)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	srcRows, err := s.db.Query(`SELECT id, group_id, transport, COALESCE(account,''), COALESCE(chat_filter,'') FROM triage_group_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer srcRows.Close()
	for srcRows.Next() {
		var src TriageSource
		if err := srcRows.Scan(&src.ID, &src.GroupID, &src.Transport, &src.Account, &src.ChatFilter); err != nil {
			return nil, err
		}
		if i, ok := byID[src.GroupID]; ok {
			out[i].Sources = append(out[i].Sources, src)
		}
	}
	return out, srcRows.Err()
}

// SaveTriageGroup creates (id==0) or replaces a group and its sources atomically.
// Owner-only.
func (s *Store) SaveTriageGroup(c Caller, g TriageGroup) (TriageGroup, error) {
	if err := requireOwner(c, "triage groups"); err != nil {
		return g, err
	}
	if strings.TrimSpace(g.Name) == "" {
		return g, fmt.Errorf("a triage group needs a name")
	}
	g.ScheduleKind = strings.ToLower(strings.TrimSpace(g.ScheduleKind))
	if g.ScheduleKind == "" {
		g.ScheduleKind = "interval"
	}
	if !validScheduleKinds[g.ScheduleKind] {
		return g, fmt.Errorf("invalid schedule kind %q (want interval|daily)", g.ScheduleKind)
	}
	if strings.TrimSpace(g.ScheduleSpec) == "" {
		return g, fmt.Errorf("a triage group needs a schedule spec (e.g. \"1h\" or \"09:00,18:00\")")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return g, err
	}
	defer tx.Rollback()

	enabled := 0
	if g.Enabled {
		enabled = 1
	}
	if g.ID == 0 {
		res, err := tx.Exec(`INSERT INTO triage_groups(name, schedule_kind, schedule_spec, enabled) VALUES(?,?,?,?)`,
			g.Name, g.ScheduleKind, g.ScheduleSpec, enabled)
		if err != nil {
			return g, err
		}
		g.ID, _ = res.LastInsertId()
	} else {
		res, err := tx.Exec(`UPDATE triage_groups SET name=?, schedule_kind=?, schedule_spec=?, enabled=? WHERE id=?`,
			g.Name, g.ScheduleKind, g.ScheduleSpec, enabled, g.ID)
		if err != nil {
			return g, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return g, fmt.Errorf("no triage group with id %d", g.ID)
		}
		if _, err := tx.Exec(`DELETE FROM triage_group_sources WHERE group_id=?`, g.ID); err != nil {
			return g, err
		}
	}

	for _, src := range g.Sources {
		t := strings.ToLower(strings.TrimSpace(src.Transport))
		if t == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO triage_group_sources(group_id, transport, account, chat_filter) VALUES(?,?,?,?)`,
			g.ID, t, nullable(src.Account), nullable(src.ChatFilter)); err != nil {
			return g, err
		}
	}
	if err := tx.Commit(); err != nil {
		return g, err
	}
	return g, nil
}

// SetTriageGroupEnabled flips a group on/off. Owner-only.
func (s *Store) SetTriageGroupEnabled(c Caller, id int64, on bool) error {
	if err := requireOwner(c, "triage groups"); err != nil {
		return err
	}
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE triage_groups SET enabled=? WHERE id=?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no triage group with id %d", id)
	}
	return nil
}

// DeleteTriageGroup removes a group and its sources (cascade). Owner-only.
func (s *Store) DeleteTriageGroup(c Caller, id int64) error {
	if err := requireOwner(c, "triage groups"); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM triage_groups WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no triage group with id %d", id)
	}
	return nil
}

// MarkTriageGroupRan records a group's last run time (RFC3339). Agent-internal.
func (s *Store) MarkTriageGroupRan(id int64, at string) error {
	_, err := s.db.Exec(`UPDATE triage_groups SET last_run_at=? WHERE id=?`, at, id)
	return err
}

// EnsureDefaultTriageGroup migrates the legacy single triage schedule into a
// "Default" group on first run (when no groups exist yet), seeding its sources
// from the connected transports. Idempotent.
func (s *Store) EnsureDefaultTriageGroup(schedule string, enabled, whatsapp bool) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM triage_groups`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	spec := strings.TrimSpace(schedule)
	if spec == "" || spec == "twice-daily" {
		spec = "1h"
	}
	g := TriageGroup{
		Name: "Default", ScheduleKind: "interval", ScheduleSpec: spec, Enabled: enabled,
		Sources: []TriageSource{{Transport: "telegram"}},
	}
	if whatsapp {
		g.Sources = append(g.Sources, TriageSource{Transport: "whatsapp"})
	}
	_, err := s.SaveTriageGroup(Owner, g)
	return err
}

func nullable(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

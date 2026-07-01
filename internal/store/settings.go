package store

import "database/sql"

// Settings are centralized scalar config (was agent-settings.json): discovery
// roots, schedules, toggles, console port/password hash, scheduler guards.
// Owner-only writes (they define what the agent can reach). Reads are open.

// GetSetting returns a setting value ("" if absent).
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// ListSettings returns all settings as a map.
func (s *Store) ListSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, COALESCE(value,'') FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting upserts a setting. Owner-only.
func (s *Store) SetSetting(c Caller, key, value string) error {
	if err := requireOwner(c, "settings"); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value,
	)
	return err
}

// DeleteSetting removes a setting. Owner-only.
func (s *Store) DeleteSetting(c Caller, key string) error {
	if err := requireOwner(c, "settings"); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, key)
	return err
}

// --- scheduler.Store adapter -------------------------------------------------

// SchedulerStore adapts the agent settings table to the scheduler's Store
// interface (Get/Set), so daily/once guards persist across restarts. It writes
// with an internal owner identity — these are the scheduler's own bookkeeping
// keys (prefixed "sched."), not user-facing settings.
type SchedulerStore struct{ S *Store }

func (a SchedulerStore) Get(key string) (string, error) { return a.S.GetSetting("sched." + key) }
func (a SchedulerStore) Set(key, value string) error {
	return a.S.SetSetting(Owner, "sched."+key, value)
}

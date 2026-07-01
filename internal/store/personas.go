package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// Persona is one agent identity (bridge/chat, triage, errand interviewer/
// producer, standup interviewer): the single source for its static seed prompt +
// model + backend. The prompt-builder injects dynamic context at runtime; the
// static scaffold lives here. Personas are crown jewels — owner-only writes.
type Persona struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Backend     string `json:"backend"`
	Model       string `json:"model"`
	SeedPrompt  string `json:"seed_prompt"`
}

const personaCols = `id, key, display_name, backend, model, seed_prompt`

func scanPersona(rows *sql.Rows) (Persona, error) {
	var p Persona
	var disp, model sql.NullString
	err := rows.Scan(&p.ID, &p.Key, &disp, &p.Backend, &model, &p.SeedPrompt)
	p.DisplayName, p.Model = disp.String, model.String
	return p, err
}

// ListPersonas returns every persona, ordered by key.
func (s *Store) ListPersonas() ([]Persona, error) {
	rows, err := s.db.Query(`SELECT ` + personaCols + ` FROM agent_personas ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Persona
	for rows.Next() {
		p, err := scanPersona(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPersona returns one persona by key (the stable identifier other tiers
// reference, e.g. "standup_interviewer").
func (s *Store) GetPersona(key string) (Persona, bool, error) {
	rows, err := s.db.Query(`SELECT `+personaCols+` FROM agent_personas WHERE key=?`, key)
	if err != nil {
		return Persona{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Persona{}, false, nil
	}
	p, err := scanPersona(rows)
	return p, true, err
}

// CreatePersona inserts a persona. Owner-only.
func (s *Store) CreatePersona(c Caller, p Persona) (Persona, error) {
	if err := requireOwner(c, "personas"); err != nil {
		return p, err
	}
	if strings.TrimSpace(p.Key) == "" || strings.TrimSpace(p.SeedPrompt) == "" {
		return p, fmt.Errorf("a persona needs a key and a seed_prompt")
	}
	if err := validateBackend(p.Backend); err != nil {
		return p, err
	}
	res, err := s.db.Exec(
		`INSERT INTO agent_personas(key, display_name, backend, model, seed_prompt, updated_at) VALUES(?,?,?,?,?,?)`,
		p.Key, p.DisplayName, strings.ToLower(p.Backend), p.Model, p.SeedPrompt, now(),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return p, fmt.Errorf("persona %q already exists", p.Key)
		}
		return p, err
	}
	p.ID, _ = res.LastInsertId()
	return p, nil
}

// UpdatePersona changes a persona's editable columns by key. Owner-only.
func (s *Store) UpdatePersona(c Caller, key string, p Persona) error {
	if err := requireOwner(c, "personas"); err != nil {
		return err
	}
	if err := validateBackend(p.Backend); err != nil {
		return err
	}
	if strings.TrimSpace(p.SeedPrompt) == "" {
		return fmt.Errorf("seed_prompt cannot be empty")
	}
	res, err := s.db.Exec(
		`UPDATE agent_personas SET display_name=?, backend=?, model=?, seed_prompt=?, updated_at=? WHERE key=?`,
		p.DisplayName, strings.ToLower(p.Backend), p.Model, p.SeedPrompt, now(), key,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no persona with key %q", key)
	}
	return nil
}

// DeletePersona removes a persona by key. Owner-only.
func (s *Store) DeletePersona(c Caller, key string) error {
	if err := requireOwner(c, "personas"); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM agent_personas WHERE key=?`, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no persona with key %q", key)
	}
	return nil
}

// UpsertPersona inserts or updates by key — the bulk/seed path (first-run
// seeding writes the whole persona set). Owner-only.
func (s *Store) UpsertPersona(c Caller, p Persona) error {
	if err := requireOwner(c, "personas"); err != nil {
		return err
	}
	if _, ok, err := s.GetPersona(p.Key); err != nil {
		return err
	} else if ok {
		return s.UpdatePersona(c, p.Key, p)
	}
	_, err := s.CreatePersona(c, p)
	return err
}

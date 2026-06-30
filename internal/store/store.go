// Package store is the agent-tier state (agent.db): the agent registry
// (personas = seed prompt + model + backend), the workspace registry, the live
// session decorations, the allowlist, and scalar settings. It is the single
// place both callers — the owner (CLI / console) and the running agent — funnel
// through, so every rule lives here:
//
//   - Validation (legal enums, FK integrity, referenced keys) is in the store,
//     never in the CLI, because the agent path would skip a CLI-side check.
//   - One caller-identity guard (owner | agent) gates every write. The crown
//     jewels — personas, allowlist, settings — are owner-only: a prompt-injected
//     errand must not be able to rewrite its own seed, add itself to the
//     allowlist, or repoint a discovery root.
//   - Derive-from-ground-truth tables (workspaces, sessions) get restricted CRUD
//     (no Create/Delete) so the staleness bug the design closed cannot reopen.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Caller identifies who performs a write. The guard below decides what each may
// touch; IPC tags the caller, and a manual `zc`/console call is owner.
type Caller string

const (
	Owner Caller = "owner"
	Agent Caller = "agent"
)

// CallerFrom maps a wire caller string to a Caller (default owner for local CLI).
func CallerFrom(s string) Caller {
	if Caller(s) == Agent {
		return Agent
	}
	return Owner
}

// ErrForbidden is returned when a caller may not write a table/column.
type ErrForbidden struct {
	Caller Caller
	What   string
}

func (e ErrForbidden) Error() string {
	return fmt.Sprintf("%s may not write %s (owner-only)", e.Caller, e.What)
}

// requireOwner rejects the agent from writing a crown-jewel table.
func requireOwner(c Caller, what string) error {
	if c != Owner {
		return ErrForbidden{Caller: c, What: what}
	}
	return nil
}

// Store is the SQLite-backed agent state.
type Store struct {
	db *sql.DB
}

// Open opens (creating + migrating) the agent store at the given path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS workspaces (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  name TEXT,
  max_role TEXT,
  discovered INTEGER NOT NULL DEFAULT 1,
  present INTEGER NOT NULL DEFAULT 1,
  ignored INTEGER NOT NULL DEFAULT 0,
  pinned INTEGER NOT NULL DEFAULT 0,
  last_used_at TEXT, created_at TEXT, updated_at TEXT
);
CREATE TABLE IF NOT EXISTS sessions (
  id INTEGER PRIMARY KEY,
  workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  backend TEXT NOT NULL,
  external_id TEXT NOT NULL,
  label TEXT,
  last_resumed_at TEXT, created_at TEXT,
  UNIQUE(backend, external_id)
);
CREATE TABLE IF NOT EXISTS agent_personas (
  id INTEGER PRIMARY KEY,
  key TEXT NOT NULL UNIQUE,
  display_name TEXT,
  backend TEXT NOT NULL,
  model TEXT,
  seed_prompt TEXT NOT NULL,
  updated_at TEXT
);
CREATE TABLE IF NOT EXISTS allowlist (
  id INTEGER PRIMARY KEY,
  platform TEXT NOT NULL, handle TEXT NOT NULL,
  max_role TEXT,
  UNIQUE(platform, handle)
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT
);
CREATE TABLE IF NOT EXISTS triage_groups (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  schedule_kind TEXT NOT NULL DEFAULT 'interval',  -- 'interval' | 'daily'
  schedule_spec TEXT NOT NULL DEFAULT '1h',        -- '1h' | '09:00,18:00'
  enabled INTEGER NOT NULL DEFAULT 1,
  last_run_at TEXT
);
CREATE TABLE IF NOT EXISTS triage_group_sources (
  id INTEGER PRIMARY KEY,
  group_id INTEGER NOT NULL REFERENCES triage_groups(id) ON DELETE CASCADE,
  transport TEXT NOT NULL,          -- 'telegram'|'whatsapp'|'instagram'
  account TEXT,                     -- which connected account (if >1)
  chat_filter TEXT                  -- optional include/exclude
);
CREATE TABLE IF NOT EXISTS reminders (
  id INTEGER PRIMARY KEY,
  requester_addr    TEXT NOT NULL,                  -- who asked: "transport|handle"
  requester_name    TEXT,                           -- display label for the requester
  target_contact_id INTEGER NOT NULL DEFAULT 0,     -- contacts ref (0 if self/none)
  target_transport  TEXT NOT NULL,                  -- 'telegram'|'whatsapp'
  target_addr       TEXT NOT NULL,                  -- who gets reminded (native reply addr)
  target_name       TEXT,                           -- display label for the reminded party
  task_text         TEXT NOT NULL,
  kind              TEXT NOT NULL DEFAULT 'oneoff',  -- 'oneoff' | 'recurring'
  recur_spec        TEXT,                           -- daily 'HH:MM' / 'weekdays HH:MM' (recurring)
  deadline_bound    INTEGER NOT NULL DEFAULT 0,
  event_at          TEXT,                           -- inferred/explicit event time (RFC3339), if any
  pre_delay_secs    INTEGER NOT NULL DEFAULT 0,      -- lead before the pre-reminder
  post_gap_secs     INTEGER NOT NULL DEFAULT 0,      -- gap to the "did you do it?" / re-ask spacing
  state             TEXT NOT NULL,                  -- scheduled|pre_reminded|awaiting_confirm|snoozed|done|missed|cancelled
  next_at           TEXT,                           -- next scheduler tick (RFC3339)
  attempts          INTEGER NOT NULL DEFAULT 0,
  last_reply        TEXT,
  created_at TEXT, updated_at TEXT
);
CREATE TABLE IF NOT EXISTS reminder_events (
  id INTEGER PRIMARY KEY,
  reminder_id INTEGER NOT NULL REFERENCES reminders(id) ON DELETE CASCADE,
  at TEXT, kind TEXT, detail TEXT   -- 'create'|'run'|'send'|'reply'|'note'|'done'|'cancel'
);`); err != nil {
		return err
	}
	// The agent-driven refactor: a reminder carries a free-text note ("carry_over")
	// the per-run agent rewrites each time, plus a run counter (safety cap). Added
	// to the existing table idempotently.
	for _, col := range []string{"carry_over TEXT", "runs INTEGER NOT NULL DEFAULT 0"} {
		if _, err := s.db.Exec(`ALTER TABLE reminders ADD COLUMN ` + col); err != nil &&
			!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// --- shared validation (in the store; both callers pass through) -------------

// validRoles are the legal max_role values (mirrors runner's role ladder). Kept
// as a set here so the store validates without importing the runner package.
var validRoles = map[string]bool{"read": true, "confirm": true, "edit": true, "full": true}

var validBackends = map[string]bool{"claude": true, "codex": true}

func validateRole(r string) error {
	if r == "" {
		return nil // unset = no cap
	}
	if !validRoles[strings.ToLower(r)] {
		return fmt.Errorf("invalid max_role %q (want read|confirm|edit|full)", r)
	}
	return nil
}

func validateBackend(b string) error {
	if !validBackends[strings.ToLower(b)] {
		return fmt.Errorf("invalid backend %q (want claude|codex)", b)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

# zcoms-agent — the AI layer

`zcoms-agent` is the AI tier of the zcoms ecosystem. It folds the old
`zcoms-bridge`, `zcoms-triage`, and `zcoms-errands` components into **one pure-Go
binary** on a single comms-harness connection, owns **`agent.db`**, runs the
scheduler, and serves **`agent.sock`** for `zc agent …` / `zc errand …` and the
team module.

`requires: [comms]` — it owns no Telegram session; it dials the comms daemon
(`zc init agent`) over IPC, so it builds and runs without cgo/TDLib. Install with
`zc install agent`.

## What it owns (`agent.db`, pure-Go `modernc.org/sqlite`)

- **personas** — one row per agent identity (bridge/triage/errand interviewer &
  producer/standup) holding its seed prompt + model + backend. The single source
  for seed text; the prompt-builders inject dynamic context around it.
- **workspaces** — discovered by scanning configured roots (never hand-authored);
  the row holds only augmentation (name, permission cap, pinned/ignored). Removal
  is Ignore, not delete; vanished repos are marked absent, never hard-deleted.
- **sessions** — enumerated **live** from the backend; the row only decorates
  (label, last-resumed). The store is never the existence authority.
- **allowlist** — who may drive the agent, enforced here (not in comms).
- **settings** — scalar config (discovery roots, schedules, toggles).

## The store guard

Every write takes a caller (`owner` | `agent`). Personas, allowlist, and settings
are **owner-only** — a prompt-injected errand cannot rewrite its own seed, add
itself to the allowlist, or repoint a discovery root. Workspaces' `max_role`
(the permission cap) is owner-only too; name/pinned/ignored are agent-writable.
Validation (enums, FK, referenced keys) lives in the store so both the CLI and
the agent path go through it.

## Published contract

Modules import `github.com/Zouriel/zcoms-agent/client` to run errands, conduct a
standup interview, and read the registries — through the one guarded seam, never
by opening `agent.db`.

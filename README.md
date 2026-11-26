# zcoms-agent — the AI layer

`zcoms-agent` is the AI tier of the zcoms ecosystem. It folds the old
`zcoms-bridge`, `zcoms-triage`, and `zcoms-errands` components into **one pure-Go
binary** on a single comms-harness connection, owns **`agent.db`**, runs the
scheduler, and serves **`agent.sock`** for `zc agent …` / `zc errand …`, `zc group …`
and `zc project …`.

`requires: [comms]` — it owns no Telegram session; it dials the comms daemon
(`zc init agent`) over IPC, so it builds and runs without cgo/TDLib. Install with
`zc install agent`.

## What it owns (`agent.db`, pure-Go `modernc.org/sqlite`)

- **personas** — one row per agent identity (bridge/triage/errand interviewer &
  producer/group/project standup) holding its seed prompt + model + backend. The single source
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

Modules import `github.com/Zouriel/zcoms-agent/client` to run errands and read
the registries (workspaces, personas, groups, projects) — through the one guarded seam, never
by opening `agent.db`.

## Groups and projects

The agent stays silent in group chats until someone calls it: an @mention of
its account, or a reply to one of its messages. It answers that one message as
a reply in the same group, in two to four sentences (700 characters at most).
A group is ignored until it is turned on. A group the agent's own account is
added to, joins or creates is turned on as a general group automatically (the
comms daemon announces it with a `group_joined` event). Otherwise:

- **General group**: `zc group on <group> [--wa]` (`zc group off` to stop).
  Calls can use web search and fetch only, in an empty scratch folder. No
  files, no shell.
- **Project group**: `zc project init`, run inside the project folder. Binds
  the folder to one group in `agent.db` (the binding is authoritative; nothing
  in the repo can change it), writes `.zcoms` (group name, `follow_tasks`,
  `[[member]]` names and aliases), keeps history in `.zcoms.d/history.db`
  (added to `.gitignore`), and adds a managed block to every
  AGENTS.md/AGENT.md/CLAUDE.md telling coding agents to send requested updates
  with `zc project say "<text>"`. Calls run in the repo with read, search and
  shell (shell withheld when the workspace `max_role` is `read`), never edit.
- **Tasks group**: the console's "Track tasks" toggle (agent line
  `group track on|off <transport> <group> [--purge]`). Creates a folder under
  the `groups_dir` setting (default `~/zcoms-groups`, named from the group),
  with `.zcoms` and `.zcoms.d/history.db` like a project, follow tasks on and
  the daily standup. Calls keep general reach (web only) but see the open
  tasks and summaries. Once the folder holds a git repo, calls become project
  calls rooted at the folder (the tightest `max_role` of the folder and its
  repos applies). Turning it off keeps the folder and history unless
  `--purge`, and puts the group back to general if it was general before.

**Member roles.** Every group member is `read`: calls get the group's normal
reach. The owner (the zcoms `owner` contact, marked by the comms daemon) is
always `write`; anyone else is made `write` per group from the console (agent
line `group role <transport> <group> <member id> read|write`). In a group
whose folder holds a repo, a write member's call also gets Edit/Write tools,
still capped by the workspace `max_role` (read or confirm: no edits). In
general groups and tasks groups without a repo, write changes nothing. The
role is decided in code from the sender id; message text never changes it,
and write never allows errands, reminders, messaging anyone, or changing
bindings, policies or personas.

Every call runs two passes: `group_understand` (top model, no tools) restates
the message in English, in any language including Dhivehi; `group` (lower
model, the group's tools) writes the reply. Both are ordinary personas.

With follow tasks on, a standup runs every day at 09:00 Indian/Maldives:
the day's chat is summarised into tasks (assignees matched by name or
`.zcoms` alias, never guessed), and each member with open tasks is tagged.
Replying "done 3" to the standup closes task 3, but only for its assignee.

Other commands, all run inside the bound folder: `zc project status`,
`remove [--purge]`, `members`, `alias add|rm <member> <alias>`,
`tasks [done|drop <id> | assign <id> <member>]`, `follow on|off`,
`standup now`, `sync`, `say <text>`.

Safety, enforced in code: calls are limited to 20 per group per hour and 8
running at once; a call never gets edit tools, never starts an errand or
reminder, and only ever replies to the calling message; agent.sock refuses
commands from any process started by a group call.

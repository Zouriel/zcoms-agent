package agentd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

// dispatch routes an agent.sock command line. agent.sock is a local 0600 socket
// reached only by the owner's CLI/console, so every write here is caller=owner
// (the crown-jewel guard distinguishes this from the in-process agent, which
// writes as caller=agent).
func (a *Agent) dispatch(text string) (string, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty command")
	}
	verb, rest := fields[0], fields[1:]
	switch verb {
	case "errand", "errands":
		return a.Errands.HandleCommand(text), nil
	case "agent":
		if len(rest) == 0 {
			return "", fmt.Errorf("usage: agent workspace|session|persona|allowlist <…>")
		}
		return a.dispatch(strings.Join(rest, " ")) // unwrap "agent <sub> …"
	case "workspace", "workspaces", "locations", "location":
		return a.workspaceCmd(rest)
	case "session", "sessions":
		return a.sessionCmd(rest)
	case "persona", "personas", "agents":
		return a.personaCmd(rest)
	case "allowlist":
		return a.allowlistCmd(rest)
	case "triage":
		return a.triageCmd(rest)
	case "settings", "setting":
		return a.settingsCmd(rest)
	case "json":
		return a.jsonQuery(rest)
	default:
		return "", fmt.Errorf("unknown command %q", verb)
	}
}

// jsonQuery returns a table's rows as JSON in the reply, for structured callers
// (the console). Still goes through the store — the console never opens agent.db.
func (a *Agent) jsonQuery(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("usage: json workspaces|personas|allowlist|settings|sessions <…>")
	}
	var v any
	var err error
	switch args[0] {
	case "workspaces":
		v, err = a.Store.ListWorkspaces(true)
	case "personas":
		v, err = a.Store.ListPersonas()
	case "allowlist":
		v, err = a.Store.ListAllow()
	case "settings":
		v, err = a.Store.ListSettings()
	case "sessions":
		if len(args) < 2 {
			return "", fmt.Errorf("usage: json sessions <workspace-id>")
		}
		id, perr := strconv.ParseInt(args[1], 10, 64)
		if perr != nil {
			return "", perr
		}
		ws, gerr := a.Store.GetWorkspace(id)
		if gerr != nil {
			return "", gerr
		}
		v, err = a.Sessions.List(ws, "claude", 25)
	default:
		return "", fmt.Errorf("unknown table %q", args[0])
	}
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func (a *Agent) settingsCmd(args []string) (string, error) {
	if len(args) == 0 || args[0] == "list" {
		vals, err := a.Store.ListSettings()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for k, v := range vals {
			if strings.HasPrefix(k, "sched.") {
				continue // scheduler bookkeeping, not user-facing
			}
			fmt.Fprintf(&b, "%s = %s\n", k, v)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	switch args[0] {
	case "get":
		if len(args) < 2 {
			return "", fmt.Errorf("usage: settings get <key>")
		}
		v, err := a.Store.GetSetting(args[1])
		return v, err
	case "set":
		if len(args) < 3 {
			return "", fmt.Errorf("usage: settings set <key> <value…>")
		}
		return "Set.", a.Store.SetSetting(store.Owner, args[1], strings.Join(args[2:], " "))
	default:
		return "", fmt.Errorf("usage: settings list|get|set")
	}
}

func (a *Agent) workspaceCmd(args []string) (string, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "list":
		wss, err := a.Registry.S.ListWorkspaces(true)
		if err != nil {
			return "", err
		}
		if len(wss) == 0 {
			return "No workspaces. Set discovery roots (settings discovery_roots) and run `zc agent workspace sync`.", nil
		}
		var b strings.Builder
		for _, w := range wss {
			flags := []string{}
			if w.Pinned {
				flags = append(flags, "pinned")
			}
			if w.Ignored {
				flags = append(flags, "ignored")
			}
			if !w.Present {
				flags = append(flags, "absent")
			}
			cap := w.MaxRole
			if cap == "" {
				cap = "—"
			}
			fmt.Fprintf(&b, "#%d %s  (%s)  cap=%s  %s\n", w.ID, nameOr(w.Name, w.Path), w.Path, cap, strings.Join(flags, ","))
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "sync":
		n, err := a.Registry.Sync()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Discovered %d repo(s).", n), nil
	case "cap":
		if len(args) < 3 {
			return "", fmt.Errorf("usage: workspace cap <id> <read|confirm|edit|full>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return "", err
		}
		role := args[2]
		return "Cap updated.", a.Store.UpdateWorkspace(store.Owner, id, nil, &role, nil, nil)
	case "pin", "unpin":
		if len(args) < 2 {
			return "", fmt.Errorf("usage: workspace pin <id>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return "", err
		}
		p := sub == "pin"
		return "Done.", a.Store.UpdateWorkspace(store.Owner, id, nil, nil, &p, nil)
	case "ignore", "unignore":
		if len(args) < 2 {
			return "", fmt.Errorf("usage: workspace ignore <id>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return "", err
		}
		ig := sub == "ignore"
		return "Done.", a.Store.SetIgnored(store.Owner, id, ig)
	default:
		return "", fmt.Errorf("usage: workspace list|sync|cap|pin|ignore")
	}
}

func (a *Agent) sessionCmd(args []string) (string, error) {
	if len(args) < 2 {
		return "", fmt.Errorf("usage: session list|resume|label <workspace-id> [...]")
	}
	sub := args[0]
	id, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return "", fmt.Errorf("workspace id: %v", err)
	}
	ws, err := a.Store.GetWorkspace(id)
	if err != nil {
		return "", err
	}
	backend := "claude"
	switch sub {
	case "list":
		sess, err := a.Sessions.List(ws, backend, 15)
		if err != nil {
			return "", err
		}
		if len(sess) == 0 {
			return "No past sessions in " + nameOr(ws.Name, ws.Path) + ".", nil
		}
		var b strings.Builder
		for _, s := range sess {
			label := ""
			if s.Label != "" {
				label = "  [" + s.Label + "]"
			}
			fmt.Fprintf(&b, "%s  %s%s\n", s.ExternalID, s.Title, label)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "resume":
		if len(args) < 3 {
			return "", fmt.Errorf("usage: session resume <workspace-id> <session-id>")
		}
		return "Marked resumed (continue it from the bridge chat).", a.Sessions.Resume(ws, backend, args[2])
	case "label":
		if len(args) < 4 {
			return "", fmt.Errorf("usage: session label <workspace-id> <session-id> <label…>")
		}
		return "Labelled.", a.Sessions.Label(store.Owner, ws, backend, args[2], strings.Join(args[3:], " "))
	default:
		return "", fmt.Errorf("usage: session list|resume|label <workspace-id>")
	}
}

func (a *Agent) personaCmd(args []string) (string, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "list":
		ps, err := a.Store.ListPersonas()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, p := range ps {
			fmt.Fprintf(&b, "%-20s backend=%-7s model=%s\n", p.Key, p.Backend, orDash(p.Model))
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "set", "edit":
		// persona set <key> backend <claude|codex> | model <m> | seed <text…>
		if len(args) < 4 {
			return "", fmt.Errorf("usage: persona set <key> backend|model|seed <value…>")
		}
		key, field, val := args[1], args[2], strings.Join(args[3:], " ")
		p, ok, err := a.Store.GetPersona(key)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no persona %q", key)
		}
		switch field {
		case "backend":
			p.Backend = val
		case "model":
			p.Model = val
		case "seed":
			p.SeedPrompt = val
		case "name":
			p.DisplayName = val
		default:
			return "", fmt.Errorf("field must be backend|model|seed|name")
		}
		return "Persona updated.", a.Store.UpdatePersona(store.Owner, key, p)
	default:
		return "", fmt.Errorf("usage: persona list|set <key> <field> <value>")
	}
}

// isRoleWord reports whether s is a role keyword (so trailing-role parsing
// doesn't swallow a handle token).
func isRoleWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read", "confirm", "edit", "full":
		return true
	}
	return false
}

func (a *Agent) allowlistCmd(args []string) (string, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "", "list":
		es, err := a.Store.ListAllow()
		if err != nil {
			return "", err
		}
		if len(es) == 0 {
			return "Allowlist is empty.", nil
		}
		var b strings.Builder
		for _, e := range es {
			fmt.Fprintf(&b, "#%d %s %s  cap=%s\n", e.ID, e.Platform, e.Handle, orDash(e.MaxRole))
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "add":
		// allowlist add [telegram|whatsapp] <handle…> [role]  (platform defaults to
		// telegram). The handle is the middle tokens joined, so a WhatsApp number
		// with spaces survives; the role is the trailing token only when it's a
		// valid role keyword.
		rest := args[1:]
		platform := "telegram"
		if len(rest) > 0 && (strings.EqualFold(rest[0], "telegram") || strings.EqualFold(rest[0], "whatsapp")) {
			platform = strings.ToLower(rest[0])
			rest = rest[1:]
		}
		role := "read"
		if len(rest) >= 2 && isRoleWord(rest[len(rest)-1]) {
			role = strings.ToLower(rest[len(rest)-1])
			rest = rest[:len(rest)-1]
		}
		if len(rest) < 1 {
			return "", fmt.Errorf("usage: allowlist add [telegram|whatsapp] <@handle|number> [read|confirm|edit|full]")
		}
		handle := runner.NormalizeAllowHandle(platform, strings.Join(rest, " "))
		_, err := a.Store.CreateAllow(store.Owner, store.AllowEntry{Platform: platform, Handle: handle, MaxRole: role})
		return "Added.", err
	case "rm", "remove":
		if len(args) < 2 {
			return "", fmt.Errorf("usage: allowlist rm <id>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return "", err
		}
		return "Removed.", a.Store.DeleteAllow(store.Owner, id)
	default:
		return "", fmt.Errorf("usage: allowlist add|rm|list")
	}
}

func (a *Agent) triageCmd(args []string) (string, error) {
	if len(args) == 0 {
		v, _ := a.Store.GetSetting("triage_schedule")
		en, _ := a.Store.GetSetting("triage_enabled")
		return fmt.Sprintf("triage enabled=%s schedule=%s", orDash(en), orDash(v)), nil
	}
	switch args[0] {
	case "on":
		return "Triage on (restart the agent to apply the schedule).", a.Store.SetSetting(store.Owner, "triage_enabled", "true")
	case "off":
		return "Triage off.", a.Store.SetSetting(store.Owner, "triage_enabled", "false")
	case "now":
		go func() { s, _ := a.buildSettings(); a.runTriageNow(s) }()
		return "Running a triage pass now…", nil
	default:
		return "Triage schedule set (restart to apply).", a.Store.SetSetting(store.Owner, "triage_schedule", args[0])
	}
}

func nameOr(name, fallback string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fallback
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

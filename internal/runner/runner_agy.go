package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// agy (the Google Antigravity CLI) gates tools by permission category, not by
// tool name: command(…) for the shell (python workflows and sub-agents
// included), read_file(…) and write_file(…) for the file tools (edit_file is a
// write), mcp(…) for MCP servers and execute_url(…) for browser actions. Deny
// rules always win over allow rules, are honoured by sub-agents, and turn the
// tool call into an error the model sees. A call that is neither allowed nor
// denied asks, and headless print mode answers an ask by ending the turn with
// no reply, so every mode below denies what it does not want instead of
// leaving it to ask. search_web is not gated by any rule, so an agy run can
// always search the web.

// Permission rule targets used across agy runs.
const (
	agyAllCommands  = "command(*)"
	agyAllReads     = "read_file(*)"
	agyAllWrites    = "write_file(*)"
	agyAllMCP       = "mcp(*)"
	agyAllBrowser   = "execute_url(*)"
	agyReadAnywhere = "read_file(/)"
)

// agyPermissions is the permissions block of an agy --settings object.
type agyPermissions struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// agySettingsArg renders p as the inline JSON --settings takes. The settings
// apply to this run only and are never saved.
func agySettingsArg(p agyPermissions) string {
	b, err := json.Marshal(struct {
		Permissions agyPermissions `json:"permissions"`
	}{p})
	if err != nil {
		// A struct of string slices always marshals; keep the run locked down
		// regardless.
		return `{"permissions":{"deny":["command(*)","read_file(*)","write_file(*)","mcp(*)","execute_url(*)"]}}`
	}
	return string(b)
}

// agyRoleArgs maps a role to agy flags, mirroring the codex and claude ladders:
//
//   - full: --dangerously-skip-permissions (everything, like codex's bypass).
//   - edit: accept-edits mode (file edits auto-approved), reads anywhere, no
//     shell, no MCP, no browser actions.
//   - read, and the confirm plan phase: reads anywhere, no writes, no shell, no
//     MCP, no browser actions.
//
// agy's plan mode is not used for read: headless, it writes a plan artifact and
// asks to proceed, and the first shell call it tries ends the turn empty.
func agyRoleArgs(role Role) []string {
	switch role {
	case RoleFull:
		return []string{"--dangerously-skip-permissions"}
	case RoleEdit:
		return []string{"--mode", "accept-edits", "--settings", agySettingsArg(agyPermissions{
			Allow: []string{agyReadAnywhere},
			Deny:  []string{agyAllCommands, agyAllMCP, agyAllBrowser},
		})}
	default:
		return []string{"--mode", "accept-edits", "--settings", agySettingsArg(agyPermissions{
			Allow: []string{agyReadAnywhere},
			Deny:  []string{agyAllWrites, agyAllCommands, agyAllMCP, agyAllBrowser},
		})}
	}
}

// agyStagingArgs is the sandboxed staging agent (triage chat, errand
// producer): accept-edits writes inside dir (the run's workspace), the fixed
// document toolchain claude's staging agent gets, and nothing else. Reads and
// writes outside dir and any other command are left to ask, which headless
// mode refuses, so they fail closed.
func agyStagingArgs() []string {
	allow := make([]string, 0, len(stagingToolAllowlist))
	for _, rule := range stagingToolAllowlist {
		if c, ok := agyCommandRule(rule); ok {
			allow = append(allow, c)
		}
	}
	return []string{"--mode", "accept-edits", "--disable-slash-commands", "--settings", agySettingsArg(agyPermissions{
		Allow: allow,
		Deny:  []string{agyAllMCP, agyAllBrowser},
	})}
}

// agyCommandRule translates a claude "Bash(prefix:*)" or "Bash(prefix *)" rule
// into agy's prefix-matched "command(prefix)". ok is false for any other rule.
func agyCommandRule(rule string) (string, bool) {
	inner, ok := strings.CutPrefix(rule, "Bash(")
	if !ok || !strings.HasSuffix(inner, ")") {
		return "", false
	}
	inner = strings.TrimSuffix(inner, ")")
	for _, suffix := range []string{":*", " *"} {
		if p, found := strings.CutSuffix(inner, suffix); found && strings.TrimSpace(p) != "" && !strings.ContainsAny(p, "*?") {
			return "command(" + strings.TrimSpace(p) + ")", true
		}
	}
	return "", false
}

// agyArgs builds the flags for one agy turn. The prompt rides on --print=
// because agy's flag parser has no "--" terminator; the = form keeps a prompt
// that starts with "-" from being read as a flag.
func agyArgs(model, prompt, resumeID string, role Role, stagingWritable bool) []string {
	args := []string{"--output-format", "json"}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	if resumeID != "" {
		// Unlike codex, agy takes the permission flags on resume too.
		args = append(args, "--conversation", resumeID)
	}
	if stagingWritable {
		args = append(args, agyStagingArgs()...)
	} else {
		args = append(args, agyRoleArgs(role)...)
	}
	return append(args, "--print="+prompt)
}

// agyJSON is the shape of `agy --output-format json -p`.
type agyJSON struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
	DeniedActions  []struct {
		Action      string `json:"action"`
		DisplayName string `json:"display_name"`
	} `json:"denied_actions"`
}

// parseAgyJSON turns agy's JSON result into a RunResult. fallbackID is kept as
// the session id when agy reports none (a failed resume).
func parseAgyJSON(out, fallbackID string) (RunResult, error) {
	if out == "" {
		return RunResult{SessionID: fallbackID}, errors.New("agy produced no output")
	}
	var parsed agyJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return RunResult{SessionID: fallbackID}, fmt.Errorf("could not parse agy output: %w", err)
	}
	id := parsed.ConversationID
	if id == "" {
		id = fallbackID
	}
	text := strings.TrimSpace(parsed.Response)
	if !strings.EqualFold(parsed.Status, "SUCCESS") {
		msg := strings.TrimSpace(parsed.Error)
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i] // drop the model list agy appends to a bad --model
		}
		if msg == "" {
			msg = strings.ToLower(parsed.Status)
		}
		return RunResult{Text: text, SessionID: id}, fmt.Errorf("agy failed: %s", msg)
	}
	if text == "" {
		if len(parsed.DeniedActions) > 0 {
			a := parsed.DeniedActions[0]
			return RunResult{SessionID: id}, fmt.Errorf("agy stopped: this role does not allow %s (%s)", a.Action, a.DisplayName)
		}
		return RunResult{SessionID: id}, errors.New("agy produced no message")
	}
	return RunResult{Text: text, SessionID: id}, nil
}

// RunAgy runs one agy turn (new conversation or resume) in dir and returns the
// reply plus the conversation id for follow-ups. An empty model leaves the
// CLI's own default in place.
func RunAgy(model, dir, prompt, resumeID string, role Role, stagingWritable bool) (RunResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), claudeRunTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, agyBin, agyArgs(model, prompt, resumeID, role, stagingWritable)...)
	cmd.Dir = dir
	cmd.Stdin = nil    // print mode must not wait on stdin
	hardenProcess(cmd) // kill the whole child group on timeout (agy spawns helpers)

	stdout := &boundedBuffer{max: maxAgentStdout}
	stderr := &boundedBuffer{max: maxAgentStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return RunResult{}, fmt.Errorf("agy timed out after %s", claudeRunTimeout)
	}
	res, err := parseAgyJSON(strings.TrimSpace(stdout.String()), resumeID)
	if err != nil && runErr != nil && strings.TrimSpace(stdout.String()) == "" {
		return res, fmt.Errorf("agy failed: %v: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	return res, err
}

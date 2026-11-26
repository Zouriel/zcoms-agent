package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// GroupCallEnv is set in the environment of every restricted (group call) run.
// The agent socket refuses commands from any process that carries it, so a
// shell a group call was given cannot start errands, reminders or project
// writes through `zc`.
const GroupCallEnv = "ZCOMS_GROUP_CALL"

// restrictedRunTimeout bounds one group-call or standup turn. A group answer is
// short; a run still going after this is stuck.
const restrictedRunTimeout = 6 * time.Minute

// ToolProfile pins exactly which built-in tools a restricted run may use. It is
// used for runs whose prompt carries third-party text (group calls, project
// standups), so nothing outside the list is reachable: no MCP servers, no
// edit/write tools, no session persistence.
type ToolProfile struct {
	// Tools are the claude built-in tool names to make available and
	// pre-approve (e.g. "WebSearch", "Read", "Bash"). Empty means no tools.
	Tools []string
	// Deny are extra --disallowedTools patterns (e.g. "Read(**/.env*)").
	Deny []string
}

// HasShell reports whether the profile includes a shell tool.
func (p ToolProfile) HasShell() bool {
	for _, t := range p.Tools {
		if t == "Bash" {
			return true
		}
	}
	return false
}

// onlyShellAndReads reports whether every tool in the profile is something a
// read-only Codex sandbox also provides (shell plus reading/searching files).
func (p ToolProfile) onlyShellAndReads() bool {
	if !p.HasShell() {
		return false
	}
	for _, t := range p.Tools {
		switch t {
		case "Bash", "Read", "Grep", "Glob", "LS":
		default:
			return false
		}
	}
	return true
}

// ErrProfileUnsupported is returned when a backend cannot honour a tool
// profile exactly. Codex always has a shell, so it can only run a profile that
// grants one; callers fall back to claude or to a canned reply.
var ErrProfileUnsupported = errors.New("backend cannot run this tool profile")

// Supports reports whether brain's backend can run profile p exactly.
func Supports(b Backend, p ToolProfile) bool {
	if b.normalize() == BackendCodex {
		return p.onlyShellAndReads()
	}
	return true
}

// writeTools are never granted to a restricted run, whatever the profile says.
var writeTools = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

// claudeRestrictedArgs builds the claude flags for a restricted one-shot run.
func claudeRestrictedArgs(model, prompt string, p ToolProfile) []string {
	tools := make([]string, 0, len(p.Tools))
	for _, t := range p.Tools {
		if !containsStr(writeTools, t) {
			tools = append(tools, t)
		}
	}
	args := []string{"-p", "--output-format", "json", "--no-session-persistence",
		// No MCP servers at all: user or connector MCP tools could message
		// people or touch accounts, which a group call must never reach.
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--tools", strings.Join(tools, ","),
		"--permission-mode", "dontAsk",
	}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	if len(tools) > 0 {
		args = append(args, "--allowedTools", strings.Join(tools, ","))
	}
	deny := append(append([]string{}, writeTools...), p.Deny...)
	args = append(args, "--disallowedTools", strings.Join(deny, ","))
	// "--" so a prompt starting with "-" is not parsed as an option.
	return append(args, "--", prompt)
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RunRestricted runs one fresh, never-resumed turn in dir with exactly the
// tools in p. Claude honours any profile; Codex only a shell-and-read profile
// (read-only sandbox, so no writes and no network) and otherwise returns
// ErrProfileUnsupported.
func RunRestricted(brain Brain, dir, prompt string, p ToolProfile) (RunResult, error) {
	if brain.Backend.normalize() == BackendCodex {
		if !p.onlyShellAndReads() {
			return RunResult{}, ErrProfileUnsupported
		}
		return runCodexReadOnly(brain.Model, dir, prompt)
	}
	args := claudeRestrictedArgs(brain.Model, prompt, p)
	return runRestrictedCmd(claudeBin, dir, args, parseClaudeJSON)
}

func runCodexReadOnly(model, dir, prompt string) (RunResult, error) {
	lastFile, err := os.CreateTemp("", "codex-last-*.txt")
	if err != nil {
		return RunResult{}, err
	}
	lastPath := lastFile.Name()
	lastFile.Close()
	defer os.Remove(lastPath)
	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "-o", lastPath,
		"-C", dir, "-s", "read-only"}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	args = append(args, "--", prompt)
	return runRestrictedCmd(codexBin, dir, args, func(string) (RunResult, error) {
		b, err := os.ReadFile(lastPath)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return RunResult{}, errors.New("codex produced no message")
		}
		return RunResult{Text: strings.TrimSpace(string(b))}, nil
	})
}

func runRestrictedCmd(bin, dir string, args []string, parse func(string) (RunResult, error)) (RunResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), restrictedRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), GroupCallEnv+"=1")
	hardenProcess(cmd)
	stdout := &boundedBuffer{max: maxAgentStdout}
	stderr := &boundedBuffer{max: maxAgentStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return RunResult{}, fmt.Errorf("restricted run timed out after %s", restrictedRunTimeout)
	}
	res, err := parse(strings.TrimSpace(stdout.String()))
	if err != nil && runErr != nil {
		return res, fmt.Errorf("%w (%v: %s)", err, runErr, strings.TrimSpace(stderr.String()))
	}
	return res, err
}

func parseClaudeJSON(out string) (RunResult, error) {
	if out == "" {
		return RunResult{}, errors.New("claude produced no output")
	}
	var parsed claudeJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return RunResult{}, fmt.Errorf("could not parse claude output: %w", err)
	}
	if parsed.IsError {
		return RunResult{Text: parsed.Result}, fmt.Errorf("claude error: %s", parsed.Subtype)
	}
	return RunResult{Text: parsed.Result}, nil
}

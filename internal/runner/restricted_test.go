package runner

import (
	"strings"
	"testing"
)

func TestClaudeRestrictedArgsNeverGrantWrites(t *testing.T) {
	args := strings.Join(claudeRestrictedArgs("", "hi", ToolProfile{Tools: []string{"Read", "Edit", "Bash"}, Deny: []string{"Read(**/.env*)"}}), " ")
	for _, want := range []string{"--strict-mcp-config", "--no-session-persistence", "--tools Read,Bash", "--permission-mode dontAsk", "Read(**/.env*)"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--tools Read,Edit") || strings.Contains(args, "--allowedTools Read,Edit") {
		t.Errorf("edit tool granted: %s", args)
	}
	none := claudeRestrictedArgs("", "hi", ToolProfile{})
	for i, a := range none {
		if a == "--tools" && none[i+1] != "" {
			t.Errorf("empty profile got tools %q", none[i+1])
		}
		if a == "--allowedTools" {
			t.Error("empty profile pre-approved tools")
		}
	}
}

// Every power tool the profile lacks is also denied by rule, sub-agent tools
// included.
func TestRestrictedDeniesUngrantedTools(t *testing.T) {
	args := claudeRestrictedArgs("", "hi", ToolProfile{Tools: []string{"WebSearch", "WebFetch"}})
	var tools, deny string
	for i, a := range args {
		switch a {
		case "--tools":
			tools = args[i+1]
		case "--disallowedTools":
			deny = args[i+1]
		}
	}
	if tools != "WebSearch,WebFetch" {
		t.Fatalf("tools = %q", tools)
	}
	for _, want := range []string{"Bash", "Read", "Grep", "Glob", "Write", "Edit", "Agent", "Task"} {
		if !strings.Contains(","+deny+",", ","+want+",") {
			t.Errorf("%s not denied: %s", want, deny)
		}
	}
}

func TestSupportsCodexOnlyWithShell(t *testing.T) {
	cases := []struct {
		p    ToolProfile
		want bool
	}{
		{ToolProfile{}, false},
		{ToolProfile{Tools: []string{"WebSearch", "WebFetch"}}, false},
		{ToolProfile{Tools: []string{"Read", "Grep", "Glob"}}, false},
		{ToolProfile{Tools: []string{"Read", "Grep", "Glob", "Bash"}}, true},
	}
	for _, c := range cases {
		if got := Supports(BackendCodex, c.p); got != c.want {
			t.Errorf("Supports(codex, %v) = %v, want %v", c.p.Tools, got, c.want)
		}
		if !Supports(BackendClaude, c.p) {
			t.Errorf("claude should support %v", c.p.Tools)
		}
	}
}

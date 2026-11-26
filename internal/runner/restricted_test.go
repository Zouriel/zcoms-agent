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

// agy can deny its shell, file and MCP tools but never web search, and its
// file rules have no globs: only a web-only profile is held exactly.
func TestSupportsAgyOnlyWeb(t *testing.T) {
	cases := []struct {
		p    ToolProfile
		want bool
	}{
		// No tools (the understand pass) runs in an empty dir of its own.
		{ToolProfile{}, true},
		{ToolProfile{Tools: []string{"WebSearch", "WebFetch"}}, true},
		{ToolProfile{Tools: []string{"WebSearch"}}, true},
		{ToolProfile{Tools: []string{"Read", "Grep", "Glob"}}, false},
		{ToolProfile{Tools: []string{"Read", "Grep", "Glob", "Bash"}, Deny: []string{"Read(**/.env*)"}}, false},
		{ToolProfile{Tools: []string{"WebSearch", "Bash"}}, false},
		{ToolProfile{Tools: []string{"WebSearch"}, Deny: []string{"WebFetch(domain:x)"}}, false},
		// Attached files: agy cannot pin a read to one file in place, so a web
		// call with attachments goes to claude; a files-only (no tools) run gets
		// copies of the files in its own empty dir.
		{ToolProfile{Tools: []string{"WebSearch", "WebFetch"}, Files: []string{"/m/a.jpg"}}, false},
		{ToolProfile{Files: []string{"/m/a.jpg"}}, true},
	}
	for _, c := range cases {
		if got := Supports(BackendAgy, c.p); got != c.want {
			t.Errorf("Supports(agy, %v/%v) = %v, want %v", c.p.Tools, c.p.Deny, got, c.want)
		}
	}
}

func TestSupportsCodexWithAttachedFiles(t *testing.T) {
	if Supports(BackendCodex, ToolProfile{Files: []string{"/m/a.jpg"}}) {
		t.Error("codex cannot run a files-only (no shell) profile")
	}
	if !Supports(BackendCodex, ToolProfile{Tools: []string{"Read", "Bash"}, Files: []string{"/m/a.jpg"}}) {
		t.Error("codex read-only sandbox can read attached files of a shell profile")
	}
}

func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// A profile with attached files and no Read grant gets the Read tool approved
// for exactly those files and nothing else.
func TestRestrictedFilesOnlyAllowExactly(t *testing.T) {
	args := claudeRestrictedArgs("", "hi", ToolProfile{Files: []string{"/m/a.jpg", "/m/b.png"}})
	tools, _ := flagValue(args, "--tools")
	if tools != "Read" {
		t.Fatalf("tools = %q, want Read", tools)
	}
	allowed, _ := flagValue(args, "--allowedTools")
	if allowed != "Read(//m/a.jpg),Read(//m/b.png)" {
		t.Fatalf("allowedTools = %q", allowed)
	}
	deny, _ := flagValue(args, "--disallowedTools")
	if strings.Contains(","+deny+",", ",Read,") {
		t.Fatalf("Read denied outright: %s", deny)
	}
	for _, want := range []string{"Bash", "Grep", "Glob", "WebSearch", "WebFetch"} {
		if !strings.Contains(","+deny+",", ","+want+",") {
			t.Errorf("%s not denied: %s", want, deny)
		}
	}

	web := claudeRestrictedArgs("", "hi", ToolProfile{Tools: []string{"WebSearch", "WebFetch"}, Files: []string{"/m/a.jpg"}})
	if allowed, _ := flagValue(web, "--allowedTools"); allowed != "WebSearch,WebFetch,Read(//m/a.jpg)" {
		t.Fatalf("web+file allowedTools = %q", allowed)
	}

	none := claudeRestrictedArgs("", "hi", ToolProfile{Tools: []string{"WebSearch", "WebFetch"}})
	if tools, _ := flagValue(none, "--tools"); strings.Contains(tools, "Read") {
		t.Fatalf("no files but Read granted: %q", tools)
	}
}

func TestFileReadRulesDropPatterns(t *testing.T) {
	got := FileReadRules([]string{"relative.jpg", "/m/*.jpg", "/m/a b.jpg", "/m/x,y.jpg", "/m/../m/ok.jpg", ""})
	if strings.Join(got, "|") != "Read(//m/ok.jpg)" {
		t.Fatalf("rules = %v", got)
	}
}

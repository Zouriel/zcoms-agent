package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// settingsOf returns the parsed --settings permissions in args, if any.
func settingsOf(t *testing.T, args []string) (agyPermissions, bool) {
	t.Helper()
	for i, a := range args {
		if a != "--settings" || i+1 >= len(args) {
			continue
		}
		var s struct {
			Permissions agyPermissions `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(args[i+1]), &s); err != nil {
			t.Fatalf("settings not JSON: %v: %s", err, args[i+1])
		}
		return s.Permissions, true
	}
	return agyPermissions{}, false
}

func has(list []string, s string) bool { return containsStr(list, s) }

// The role ladder matches codex's: only full skips permissions; read and the
// confirm plan phase can neither write nor run a shell; edit can write but not
// run a shell.
func TestAgyRoleArgs(t *testing.T) {
	full := agyRoleArgs(RoleFull)
	if !has(full, "--dangerously-skip-permissions") {
		t.Fatalf("full = %v", full)
	}
	for _, role := range []Role{RoleRead, RoleConfirm, RoleEdit} {
		args := agyRoleArgs(role)
		if has(args, "--dangerously-skip-permissions") {
			t.Errorf("%s skips permissions: %v", role, args)
		}
		p, ok := settingsOf(t, args)
		if !ok {
			t.Fatalf("%s has no permission rules: %v", role, args)
		}
		for _, deny := range []string{agyAllCommands, agyAllMCP, agyAllBrowser} {
			if !has(p.Deny, deny) {
				t.Errorf("%s does not deny %s: %v", role, deny, p.Deny)
			}
		}
		if wantWrite := role == RoleEdit; has(p.Deny, agyAllWrites) == wantWrite {
			t.Errorf("%s write deny = %v", role, has(p.Deny, agyAllWrites))
		}
	}
}

func TestAgyArgsPromptResumeModel(t *testing.T) {
	args := agyArgs("gemini-4-neon-high", "-starts with a dash", "conv-1", RoleRead, false)
	if last := args[len(args)-1]; last != "--print=-starts with a dash" {
		t.Fatalf("prompt arg = %q", last)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--output-format json", "--model gemini-4-neon-high", "--conversation conv-1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if fresh := strings.Join(agyArgs("", "hi", "", RoleFull, false), " "); strings.Contains(fresh, "--model") || strings.Contains(fresh, "--conversation") {
		t.Errorf("fresh default run carries model or resume: %s", fresh)
	}
}

// The staging agent gets claude's fixed render toolchain as command rules and
// nothing auto-approved beyond it.
func TestAgyStagingArgs(t *testing.T) {
	args := agyArgs("", "hi", "", RoleRead, true)
	if has(args, "--dangerously-skip-permissions") || !has(args, "--disable-slash-commands") {
		t.Fatalf("staging args = %v", args)
	}
	p, ok := settingsOf(t, args)
	if !ok {
		t.Fatal("no settings")
	}
	for _, want := range []string{"command(pandoc)", "command(chromium)", "command(cp)"} {
		if !has(p.Allow, want) {
			t.Errorf("allow missing %s: %v", want, p.Allow)
		}
	}
	if has(p.Allow, agyAllCommands) || has(p.Allow, agyReadAnywhere) || !has(p.Deny, agyAllMCP) {
		t.Errorf("staging too broad: %+v", p)
	}
}

func TestAgyCommandRule(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"Bash(pandoc:*)", "command(pandoc)", true},
		{"Bash(zc *)", "command(zc)", true},
		{"Bash(*)", "", false},
		{"Read(**/.env*)", "", false},
		{"Bash(rm)", "", false},
	}
	for _, c := range cases {
		got, ok := agyCommandRule(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("agyCommandRule(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseAgyJSON(t *testing.T) {
	ok := `{"conversation_id":"c1","status":"SUCCESS","response":"Hi!\n","num_turns":1}`
	res, err := parseAgyJSON(ok, "")
	if err != nil || res.Text != "Hi!" || res.SessionID != "c1" {
		t.Fatalf("success = %+v, %v", res, err)
	}

	bad := `{"conversation_id":"","status":"ERROR","response":"","error":"invalid model selection: model x is not recognized\nAvailable models:\n  Gemini"}`
	res, err = parseAgyJSON(bad, "keep")
	if err == nil || !strings.Contains(err.Error(), "invalid model selection") || strings.Contains(err.Error(), "Available") {
		t.Fatalf("error = %v", err)
	}
	if res.SessionID != "keep" {
		t.Errorf("failed resume lost its id: %+v", res)
	}

	denied := `{"conversation_id":"c2","status":"SUCCESS","response":"","denied_actions":[{"action":"command","display_name":"Bash"}]}`
	if _, err := parseAgyJSON(denied, ""); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("denied = %v", err)
	}
	if _, err := parseAgyJSON("not json", ""); err == nil {
		t.Fatal("garbage parsed")
	}
	if _, err := parseAgyJSON("", ""); err == nil {
		t.Fatal("empty parsed")
	}
}

func TestListAgySessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation_metadata.json")
	meta := `{"conversations":{
	  "a":{"summary":{"ID":"a","Title":"","Preview":"fix the build","UpdatedAt":"2026-11-20T10:00:00Z","WorkspaceURIs":["file:///work/proj"]},"is_internal":false},
	  "b":{"summary":{"ID":"b","Title":"Newer","UpdatedAt":"2026-11-21T10:00:00Z","WorkspaceURIs":["file:///other","file:///work/proj"]},"is_internal":false},
	  "c":{"summary":{"ID":"c","Title":"Elsewhere","UpdatedAt":"2026-11-22T10:00:00Z","WorkspaceURIs":["file:///work/proj2"]},"is_internal":false},
	  "d":{"summary":{"ID":"d","Title":"Internal","UpdatedAt":"2026-11-23T10:00:00Z","WorkspaceURIs":["file:///work/proj"]},"is_internal":true}
	}}`
	if err := os.WriteFile(path, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := listAgySessionsFrom(path, "/work/proj", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" || got[1].Title != "fix the build" {
		t.Fatalf("sessions = %+v", got)
	}
	if !got[0].Modified.Equal(time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("modified = %v", got[0].Modified)
	}
	if one, _ := listAgySessionsFrom(path, "/work/proj", 1); len(one) != 1 {
		t.Errorf("limit ignored: %d", len(one))
	}
	if none, err := listAgySessionsFrom(filepath.Join(t.TempDir(), "missing.json"), "/x", 0); err != nil || none != nil {
		t.Errorf("missing index = %v, %v", none, err)
	}
}

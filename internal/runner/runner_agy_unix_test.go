//go:build !windows

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgy installs a stand-in agy that records its argv and environment and
// prints a canned JSON result, and points agyBin at it for the test.
func fakeAgy(t *testing.T, result string) (argsFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, envFile = filepath.Join(dir, "args"), filepath.Join(dir, "env")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argsFile +
		"\nenv > " + envFile + "\ncat <<'JSON'\n" + result + "\nJSON\n"
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := agyBin
	agyBin = bin
	t.Cleanup(func() { agyBin = old })
	return argsFile, envFile
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestRunAgentDispatchesToAgy(t *testing.T) {
	argsFile, _ := fakeAgy(t, `{"conversation_id":"conv-9","status":"SUCCESS","response":"done"}`)
	res, err := RunAgent(Brain{Backend: BackendAgy, Model: "gemini-4-hydrogen-medium"}, t.TempDir(), "hello", "conv-8", RoleFull, false)
	if err != nil || res.Text != "done" || res.SessionID != "conv-9" {
		t.Fatalf("RunAgent = %+v, %v", res, err)
	}
	args := readLines(t, argsFile)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--model gemini-4-hydrogen-medium", "--conversation conv-8", "--dangerously-skip-permissions", "--print=hello"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, args)
		}
	}
}

// A web-only group call runs on agy with every non-web category denied, no
// auto-approval, slash commands off and the group-call marker in its env.
func TestRunRestrictedAgyWebOnly(t *testing.T) {
	argsFile, envFile := fakeAgy(t, `{"conversation_id":"c","status":"SUCCESS","response":"Paris"}`)
	res, err := RunRestricted(Brain{Backend: BackendAgy}, t.TempDir(), "/clear what is the capital?", ToolProfile{Tools: []string{"WebSearch", "WebFetch"}})
	if err != nil || res.Text != "Paris" {
		t.Fatalf("RunRestricted = %+v, %v", res, err)
	}
	args := readLines(t, argsFile)
	if has(args, "--dangerously-skip-permissions") || !has(args, "--disable-slash-commands") {
		t.Fatalf("argv = %v", args)
	}
	p, ok := settingsOf(t, args)
	if !ok {
		t.Fatal("no settings")
	}
	for _, deny := range []string{agyAllCommands, agyAllReads, agyAllWrites, agyAllMCP, agyAllBrowser} {
		if !has(p.Deny, deny) {
			t.Errorf("not denied: %s (%v)", deny, p.Deny)
		}
	}
	if len(p.Allow) != 0 {
		t.Errorf("restricted run allows %v", p.Allow)
	}
	if !strings.Contains(strings.Join(readLines(t, envFile), "\n"), GroupCallEnv+"=1") {
		t.Error("group-call marker missing from env")
	}

	if _, err := RunRestricted(Brain{Backend: BackendAgy}, t.TempDir(), "hi", ToolProfile{}); err != ErrProfileUnsupported {
		t.Fatalf("tool-less profile on agy = %v, want ErrProfileUnsupported", err)
	}
}

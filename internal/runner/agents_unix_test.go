//go:build !windows

package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExec(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestAgentAvailableFindsLocalBin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	if AgentAvailable(BackendClaude) {
		t.Fatal("claude reported available with no binary anywhere")
	}
	writeExec(t, home+"/.local/bin", "claude")
	if !AgentAvailable(BackendClaude) {
		t.Fatal("claude in ~/.local/bin not found when PATH lacks it")
	}
}

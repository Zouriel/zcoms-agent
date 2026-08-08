package personas

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := SeedDefaults(s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

func installed(b runner.Backend) bool {
	_, err := exec.LookPath(string(b))
	return err == nil
}

// A persona's stored backend + model is what actually drives its runs. This is
// the invariant that broke when the runtime asked for task names ("chat",
// "errands") that no persona row provides, so every such run silently landed on
// claude while the console showed codex.
func TestBrainFollowsThePersonaRow(t *testing.T) {
	if !installed(runner.BackendCodex) || !installed(runner.BackendClaude) {
		t.Skip("needs both CLIs installed to tell a real choice from a fallback")
	}
	s := openTest(t)
	p, _, err := s.GetPersona(Bridge)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	p.Backend, p.Model = "codex", "gpt-5"
	if err := s.UpdatePersona(store.Owner, Bridge, p); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := Brain(s, Bridge)
	if got.Backend != runner.BackendCodex || got.Model != "gpt-5" {
		t.Fatalf("Brain(bridge) = %+v, want codex/gpt-5", got)
	}
	// An explicit override (a user's pinned agent) wins, and drops the model,
	// which named a model of the backend the persona asked for.
	if got := BrainWith(s, Bridge, runner.BackendClaude); got.Backend != runner.BackendClaude || got.Model != "" {
		t.Fatalf("BrainWith(bridge, claude) = %+v, want claude with no model", got)
	}
}

// Every persona the runtime resolves must have a row, or its runs fall back to
// whichever CLI is installed and the console's choice is silently ignored.
func TestEveryRuntimePersonaHasARow(t *testing.T) {
	s := openTest(t)
	for _, key := range []string{
		Bridge, Workspace, Triage, ErrandInterviewer, ErrandProducer, Guard,
		Reminders, Morning, Reschedule, Lineup,
	} {
		if _, ok, err := s.GetPersona(key); err != nil {
			t.Fatalf("get %s: %v", key, err)
		} else if !ok {
			t.Errorf("persona %q is resolved at runtime but has no seeded row", key)
		}
	}
}

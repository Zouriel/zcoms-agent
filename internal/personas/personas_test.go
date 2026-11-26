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

// Group personas have a top understand model and a lower work model on claude
// and agy, and only claude's are seeded.
func TestGroupDefaultModels(t *testing.T) {
	for _, b := range []runner.Backend{runner.BackendClaude, runner.BackendAgy} {
		u, w := DefaultModel(b, GroupUnderstand), DefaultModel(b, Group)
		if u == "" || w == "" || u == w || DefaultModel(b, ProjectStandup) != w {
			t.Errorf("%s: understand=%q work=%q", b, u, w)
		}
	}
	if DefaultModel(runner.BackendAgy, Bridge) != "" {
		t.Error("bridge should run on the CLI default")
	}
	s := openTest(t)
	if p, _, _ := s.GetPersona(GroupUnderstand); p.Backend != "claude" || p.Model != DefaultModel(runner.BackendClaude, GroupUnderstand) {
		t.Errorf("seeded understand = %s/%s", p.Backend, p.Model)
	}
}

// voice_transcribe is seeded on agy (the backend that hears audio) with an
// agy model, and stays editable like any other row.
func TestVoiceTranscribeSeededOnAgy(t *testing.T) {
	s := openTest(t)
	p, ok, err := s.GetPersona(VoiceTranscribe)
	if err != nil || !ok {
		t.Fatalf("voice_transcribe not seeded: %v", err)
	}
	if p.Backend != "agy" || p.Model == "" || p.Model != DefaultModel(runner.BackendAgy, VoiceTranscribe) || !ModelFits(runner.BackendAgy, p.Model) {
		t.Fatalf("seeded voice_transcribe = %s/%s", p.Backend, p.Model)
	}
	if p.SeedPrompt == "" {
		t.Fatal("no seed")
	}
	if DefaultBackend(Bridge) != runner.BackendClaude {
		t.Fatal("other personas must still seed on claude")
	}
}

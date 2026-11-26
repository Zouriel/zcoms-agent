package agentd

import (
	"path/filepath"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

func testAgent(t *testing.T) *Agent {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := personas.SeedDefaults(s); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return &Agent{Store: s}
}

// A model must be clearable: "Default" in the console means no --model flag at
// all, so `persona set <key> model` with no value has to succeed and empty the
// column rather than fail as a usage error.
func TestPersonaSetModelClears(t *testing.T) {
	a := testAgent(t)

	if _, err := a.personaCmd([]string{"set", personas.Bridge, "model", "gpt-5.6-sol"}); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if p, _, _ := a.Store.GetPersona(personas.Bridge); p.Model != "gpt-5.6-sol" {
		t.Fatalf("model = %q, want gpt-5.6-sol", p.Model)
	}

	if _, err := a.personaCmd([]string{"set", personas.Bridge, "model"}); err != nil {
		t.Fatalf("clear model: %v", err)
	}
	if p, _, _ := a.Store.GetPersona(personas.Bridge); p.Model != "" {
		t.Fatalf("model = %q, want it cleared", p.Model)
	}
}

// Backend has no empty form — the store would reject it, so the command says so
// instead of writing a persona no CLI can run.
func TestPersonaSetBackendRejectsEmpty(t *testing.T) {
	a := testAgent(t)
	if _, err := a.personaCmd([]string{"set", personas.Bridge, "backend"}); err == nil {
		t.Fatal("clearing backend should be rejected")
	}
	if p, _, _ := a.Store.GetPersona(personas.Bridge); p.Backend == "" {
		t.Fatal("backend was cleared despite the error")
	}
}

// A model belongs to one CLI, so moving a persona to agy moves its model to
// agy's default for that persona, and moving it back restores claude's.
func TestPersonaSetBackendMovesModel(t *testing.T) {
	a := testAgent(t)
	if _, err := a.personaCmd([]string{"set", personas.Group, "backend", "agy"}); err != nil {
		t.Fatalf("set backend agy: %v", err)
	}
	p, _, _ := a.Store.GetPersona(personas.Group)
	if p.Backend != "agy" || p.Model != personas.DefaultModel(runner.BackendAgy, personas.Group) || p.Model == "" {
		t.Fatalf("after agy: backend=%q model=%q", p.Backend, p.Model)
	}
	if _, err := a.personaCmd([]string{"set", personas.Group, "backend", "claude"}); err != nil {
		t.Fatalf("set backend claude: %v", err)
	}
	if p, _, _ := a.Store.GetPersona(personas.Group); p.Model != "sonnet" {
		t.Fatalf("after claude: model=%q, want sonnet", p.Model)
	}
	// Re-setting the same backend keeps a hand-picked model.
	if _, err := a.personaCmd([]string{"set", personas.Group, "model", "opus"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.personaCmd([]string{"set", personas.Group, "backend", "claude"}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := a.Store.GetPersona(personas.Group); p.Model != "opus" {
		t.Fatalf("same backend reset the model to %q", p.Model)
	}
}

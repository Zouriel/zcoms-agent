package agentd

import (
	"path/filepath"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/personas"
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

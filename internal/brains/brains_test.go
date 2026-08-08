package brains

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The help text is wrapped across lines, so the aliases must be recovered from
// the joined block rather than a single line. Full model names are examples of
// the pinned form, not a catalogue, so they are not offered as choices.
func TestClaudeHelpAliases(t *testing.T) {
	help := strings.Join([]string{
		"  --mcp-config <configs...>             Load MCP servers from JSON files",
		"  --model <model>                       Model for the current session. Provide",
		"                                        an alias for the latest model (e.g.",
		"                                        'fable', 'opus', or 'sonnet') or a",
		"                                        model's full name (e.g.",
		"                                        'claude-fable-5').",
		"  -n, --name <name>                     Set a display name for this session",
	}, "\n")

	block := modelHelpBlock(help)
	if block == "" {
		t.Fatal("the --model help block was not found")
	}
	if strings.Contains(block, "display name") {
		t.Fatalf("block ran past the --model entry into the next flag: %q", block)
	}

	var got []string
	seen := map[string]bool{}
	for _, m := range quoted.FindAllStringSubmatch(block, -1) {
		tok := m[1]
		if strings.HasPrefix(tok, "claude-") || strings.Contains(tok, " ") || seen[tok] {
			continue
		}
		seen[tok] = true
		got = append(got, tok)
	}
	want := []string{"fable", "opus", "sonnet"}
	if len(got) != len(want) {
		t.Fatalf("aliases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("aliases = %v, want %v", got, want)
		}
	}
}

// A CLI with no --model flag yields no block rather than a false match.
func TestModelHelpBlockAbsent(t *testing.T) {
	if block := modelHelpBlock("  --verbose  Be loud\n  --quiet  Be quiet\n"); block != "" {
		t.Fatalf("expected no block, got %q", block)
	}
}

// codex's own cache carries hidden and non-API entries; offering those would put
// dead options in the picker. Order follows codex's priority field.
func TestCodexModelsFiltersAndOrders(t *testing.T) {
	dir := t.TempDir()
	raw := `{"models":[
	  {"slug":"gpt-5.4","display_name":"GPT-5.4","visibility":"list","supported_in_api":true,"priority":16},
	  {"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","visibility":"list","supported_in_api":true,"priority":1},
	  {"slug":"gpt-5.6-sol-wm","display_name":"hidden","visibility":"hide","supported_in_api":false,"priority":1},
	  {"slug":"codex-auto-review","display_name":"internal","visibility":"hide","supported_in_api":true,"priority":43}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", dir)

	models, source := codexModels()
	if source != "codex model cache" {
		t.Fatalf("source = %q", source)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want the 2 listed API models", models)
	}
	if models[0].ID != "gpt-5.6-sol" || models[1].ID != "gpt-5.4" {
		t.Fatalf("models not in codex priority order: %+v", models)
	}
	if models[0].Display != "GPT-5.6-Sol" {
		t.Fatalf("display name lost: %+v", models[0])
	}
}

// A missing cache is not an error: the backend is still installed and usable on
// its default model, and the owner can type a model by hand.
func TestCodexModelsMissingCache(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	models, source := codexModels()
	if models != nil {
		t.Fatalf("expected no models, got %+v", models)
	}
	if source == "" {
		t.Fatal("source should explain why the list is empty")
	}
}

// Discovery against the CLIs actually installed here. Skips where they aren't
// (CI), so it documents the real shape without being environment-dependent.
func TestLiveDiscovery(t *testing.T) {
	for _, b := range List() {
		if !b.Installed {
			t.Logf("%s: not installed", b.Name)
			continue
		}
		t.Logf("%s (%s): %d model(s)", b.Name, b.Source, len(b.Models))
		for _, m := range b.Models {
			t.Logf("    %-16s %s", m.ID, m.Display)
			if m.ID == "" {
				t.Errorf("%s returned a model with an empty id", b.Name)
			}
		}
		if len(b.Models) == 0 {
			t.Logf("    (none discovered — %s)", b.Source)
		}
	}
}

// Package brains discovers which agent CLIs are installed on this machine and
// which models each one accepts, so the console can offer the real choices
// instead of a hardcoded list that drifts as the CLIs are upgraded.
//
// Nothing here is authoritative about what Anthropic or OpenAI publish: each
// list is read from the installed CLI itself (codex caches its model catalogue
// on disk; claude documents its accepted aliases in `--model` help), so a CLI
// upgrade updates the console with no zcoms release. Every list is advisory —
// the owner can always type a model the discovery missed, and the CLI has the
// final say at run time.
package brains

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// ttl is how long a discovery result is reused. Discovery shells out to a CLI
// and reads files, and the console polls, so this keeps it off the hot path
// while still picking up a CLI upgrade within a few minutes.
const ttl = 5 * time.Minute

// helpTimeout bounds the `claude --help` probe so a wedged CLI cannot stall the
// agent's command socket.
const helpTimeout = 10 * time.Second

// Model is one selectable model for a backend. ID is what gets passed to the
// CLI's --model flag; an empty ID is never returned (the caller represents "the
// CLI's own default" by storing no model at all).
type Model struct {
	ID      string `json:"id"`
	Display string `json:"display"`
}

// Backend is one agent CLI and what it offers. Models is nil when discovery
// found nothing, which is not an error: the owner can still type a model by
// hand, and Source says where the (possibly empty) list came from.
type Backend struct {
	Name      string  `json:"name"` // "claude" | "codex"
	Installed bool    `json:"installed"`
	Models    []Model `json:"models"`
	Source    string  `json:"source"`
}

var (
	mu       sync.Mutex
	cached   []Backend
	cachedAt time.Time
)

// List returns every known backend, whether it is installed, and the models it
// accepts. Results are cached for a few minutes; call Invalidate to force a
// fresh probe.
func List() []Backend {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil && time.Since(cachedAt) < ttl {
		return cached
	}
	cached = discover()
	cachedAt = time.Now()
	return cached
}

// Invalidate drops the cached discovery so the next List re-probes. Called after
// a CLI is installed or upgraded.
func Invalidate() {
	mu.Lock()
	cached, cachedAt = nil, time.Time{}
	mu.Unlock()
}

func discover() []Backend {
	out := []Backend{}
	for _, b := range []runner.Backend{runner.BackendClaude, runner.BackendCodex} {
		entry := Backend{Name: string(b), Installed: runner.AgentAvailable(b)}
		if !entry.Installed {
			entry.Source = "not installed"
			out = append(out, entry)
			continue
		}
		switch b {
		case runner.BackendCodex:
			entry.Models, entry.Source = codexModels()
		default:
			entry.Models, entry.Source = claudeModels()
		}
		out = append(out, entry)
	}
	return out
}

// --- codex ------------------------------------------------------------------

// codexCache is the slice of ~/.codex/models_cache.json we read. The codex CLI
// refreshes that file itself, so it is the closest thing to a live catalogue we
// can read without network access or an API key.
type codexCache struct {
	Models []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
		Visibility  string `json:"visibility"`
		SupportedIn bool   `json:"supported_in_api"`
		Priority    int    `json:"priority"`
	} `json:"models"`
}

// codexModels reads codex's own model cache, keeping the models it lists for
// selection and can serve over the API (the file also carries hidden and
// internal entries, which would be dead options in a picker).
func codexModels() ([]Model, string) {
	path := filepath.Join(codexHome(), "models_cache.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "codex model cache unavailable"
	}
	var c codexCache
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, "codex model cache unreadable"
	}
	type entry struct {
		Model
		priority int
	}
	var keep []entry
	for _, m := range c.Models {
		if m.Visibility != "list" || !m.SupportedIn || strings.TrimSpace(m.Slug) == "" {
			continue
		}
		display := strings.TrimSpace(m.DisplayName)
		if display == "" {
			display = m.Slug
		}
		keep = append(keep, entry{Model{ID: m.Slug, Display: display}, m.Priority})
	}
	// codex orders its own picker by priority; match it so the console shows the
	// same order the CLI would.
	sort.SliceStable(keep, func(i, j int) bool {
		if keep[i].priority != keep[j].priority {
			return keep[i].priority < keep[j].priority
		}
		return keep[i].ID < keep[j].ID
	})
	models := make([]Model, 0, len(keep))
	for _, e := range keep {
		models = append(models, e.Model)
	}
	if len(models) == 0 {
		return nil, "codex model cache had no selectable models"
	}
	return models, "codex model cache"
}

// codexHome resolves codex's config dir the same way the CLI does.
func codexHome() string {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

// --- claude -----------------------------------------------------------------

// claudeKnownAliases are long-standing aliases the CLI accepts. They fill the
// gap when the help text mentions only some of them (it currently documents
// three by way of example), and stand in entirely when the help can't be read.
// Aliases, not pinned ids: each resolves to the current model in its family, so
// the list cannot go stale the way a full model id would.
var claudeKnownAliases = []string{"opus", "sonnet", "haiku"}

// quoted matches the 'single-quoted' examples in the CLI's help text.
var quoted = regexp.MustCompile(`'([^']+)'`)

// claudeModels asks the installed claude CLI what its --model flag accepts. The
// help text advertises aliases for the latest model in each family (plus one
// full-name example), which is exactly the right granularity to offer: an alias
// keeps pointing at the current model as new ones ship. A pinned full id is
// still valid, so the console lets the owner type one.
func claudeModels() ([]Model, string) {
	aliases, err := claudeHelpAliases()
	if err != nil || len(aliases) == 0 {
		return aliasModels(claudeKnownAliases), "built-in alias list (claude help unreadable)"
	}
	source := "claude --model help"
	seen := map[string]bool{}
	for _, a := range aliases {
		seen[a] = true
	}
	for _, a := range claudeKnownAliases {
		if !seen[a] {
			aliases = append(aliases, a)
			source = "claude --model help + built-in aliases"
		}
	}
	return aliasModels(aliases), source
}

// claudeHelpAliases runs `claude --help` and pulls the quoted aliases out of the
// --model entry. Full names (claude-…) are dropped: they are examples of the
// pinned form, not a catalogue, and offering one stale id is worse than none.
func claudeHelpAliases() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), helpTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, runner.AgentBin("claude"), "--help").Output()
	if err != nil {
		return nil, err
	}
	block := modelHelpBlock(string(out))
	if block == "" {
		return nil, nil
	}
	var aliases []string
	seen := map[string]bool{}
	for _, m := range quoted.FindAllStringSubmatch(block, -1) {
		tok := strings.TrimSpace(m[1])
		if tok == "" || strings.Contains(tok, " ") || strings.HasPrefix(tok, "claude-") || seen[tok] {
			continue
		}
		seen[tok] = true
		aliases = append(aliases, tok)
	}
	return aliases, nil
}

// modelHelpBlock extracts the wrapped help text for --model: from the line that
// introduces the flag up to the next flag line. Returns "" when the flag is
// absent (an older or very different CLI).
func modelHelpBlock(help string) string {
	lines := strings.Split(help, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--model ") {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(lines[start])
	for _, line := range lines[start+1:] {
		// A new flag entry ends the block; continuation lines are pure prose.
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "-") {
			break
		}
		b.WriteString(" ")
		b.WriteString(strings.TrimSpace(line))
	}
	return b.String()
}

// aliasModels labels each alias with what it means, so the console can show
// "opus (latest opus)" rather than a bare word.
func aliasModels(aliases []string) []Model {
	models := make([]Model, 0, len(aliases))
	for _, a := range aliases {
		models = append(models, Model{ID: a, Display: a + " (latest " + a + ")"})
	}
	return models
}

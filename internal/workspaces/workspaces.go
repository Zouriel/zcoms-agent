// Package workspaces is the agent's workspace registry: it keeps agent.db's
// workspaces table in sync with reality on disk. The principle is
// derive-from-ground-truth: repos are DISCOVERED by scanning configured roots,
// never hand-authored, so the list can't drift from what's actually there. The
// store row only holds the augmentation (friendly name, permission cap, pinned/
// ignored); discovery owns path/present.
package workspaces

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// rootsKey is the settings key holding the discovery roots (newline/comma/colon
// separated absolute paths to scan for git repos).
const rootsKey = "discovery_roots"

// Registry wraps the store with discovery behavior.
type Registry struct {
	S *store.Store
}

func New(s *store.Store) *Registry { return &Registry{S: s} }

// Roots returns the configured discovery roots.
func (r *Registry) Roots() ([]string, error) {
	raw, err := r.S.GetSetting(rootsKey)
	if err != nil {
		return nil, err
	}
	return splitRoots(raw), nil
}

// SetRoots replaces the discovery roots (owner-only via the store guard).
func (r *Registry) SetRoots(c store.Caller, roots []string) error {
	return r.S.SetSetting(c, rootsKey, strings.Join(roots, "\n"))
}

func splitRoots(raw string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == ',' || r == ':'
	}) {
		if p := strings.TrimSpace(f); p != "" {
			out = append(out, expandHome(p))
		}
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Sync scans every configured root for git repos and reconciles the store:
// upsert discovered repos (set present, derive name), mark vanished ones
// present=0 (never hard-delete), and skip ignored paths so a manual hide sticks.
// Returns the count of repos currently present. Discovery is the workspaces
// table's only Create path — that's why the store exposes no public Create.
func (r *Registry) Sync() (int, error) {
	roots, err := r.Roots()
	if err != nil {
		return 0, err
	}

	// Existing ignored paths must not be re-registered as present.
	all, err := r.S.ListWorkspaces(true)
	if err != nil {
		return 0, err
	}
	ignored := map[string]bool{}
	for _, w := range all {
		if w.Ignored {
			ignored[w.Path] = true
		}
	}

	present := map[string]bool{}
	for _, root := range roots {
		repos, err := findGitRepos(root)
		if err != nil {
			continue // a missing/unreadable root shouldn't fail the whole sync
		}
		for _, repo := range repos {
			if ignored[repo] {
				continue // honor a manual hide
			}
			if err := r.S.UpsertDiscovered(repo, deriveName(repo)); err != nil {
				return 0, err
			}
			present[repo] = true
		}
	}
	if err := r.S.MarkAbsent(present); err != nil {
		return 0, err
	}
	return len(present), nil
}

// findGitRepos returns directories under root (root itself + its immediate
// children) that contain a .git entry. One level deep matches how people lay out
// a code directory (~/src/<repo>), without an expensive full-tree walk.
func findGitRepos(root string) ([]string, error) {
	var out []string
	if isGitRepo(root) {
		out = append(out, root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		child := filepath.Join(root, e.Name())
		if isGitRepo(child) {
			out = append(out, child)
		}
	}
	return out, nil
}

func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

func deriveName(repo string) string { return filepath.Base(repo) }

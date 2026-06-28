// Package sessions is the agent's session manager. A Session is one agent run
// anchored to a workspace, started fresh or resumed from a prior Claude Code /
// Codex run. Existence is enumerated LIVE from the backend at query time — the
// store never caches which sessions exist (that was the staleness bug). The
// store row only decorates a live session with a label / last-resumed stamp.
package sessions

import (
	"sort"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

// Session is a live backend session joined with its store decoration.
type Session struct {
	ExternalID string `json:"external_id"`
	Title      string `json:"title"`
	Label      string `json:"label"` // augmentation from the store (may be "")
	Backend    string `json:"backend"`
}

// Manager enumerates and decorates sessions for a workspace.
type Manager struct {
	S *store.Store
}

func New(s *store.Store) *Manager { return &Manager{S: s} }

// List enumerates the workspace's actual sessions from the backend, then joins
// the store's labels onto them. The store is never the existence authority — if
// a session vanished from the backend it simply isn't returned, no stale row can
// resurrect it.
func (m *Manager) List(ws store.Workspace, backend string, limit int) ([]Session, error) {
	live, err := runner.ListSessionsFor(runner.Backend(backend), ws.Path, limit)
	if err != nil {
		return nil, err
	}
	decs, err := m.S.ListSessionDecorations(ws.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(live))
	for _, s := range live {
		sess := Session{ExternalID: s.ID, Title: s.Title, Backend: backend}
		if d, ok := decs[s.ID]; ok {
			sess.Label = d.Label
		}
		out = append(out, sess)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

// Label attaches/updates a label on a live session (decoration only).
func (m *Manager) Label(c store.Caller, ws store.Workspace, backend, externalID, label string) error {
	return m.S.SetLabel(c, ws.ID, backend, externalID, label)
}

// Resume marks a session as just-resumed (decoration); the caller then drives
// the backend with the external id. Resume does not create a session.
func (m *Manager) Resume(ws store.Workspace, backend, externalID string) error {
	return m.S.TouchResumed(ws.ID, backend, externalID)
}

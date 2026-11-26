package runner

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// agyMetadata is the slice of agy's conversation index
// (~/.gemini/antigravity-cli/cache/conversation_metadata.json) we read.
type agyMetadata struct {
	Conversations map[string]struct {
		Summary struct {
			ID            string    `json:"ID"`
			Title         string    `json:"Title"`
			Preview       string    `json:"Preview"`
			UpdatedAt     time.Time `json:"UpdatedAt"`
			WorkspaceURIs []string  `json:"WorkspaceURIs"`
			Internal      bool      `json:"Internal"`
		} `json:"summary"`
		IsInternal bool `json:"is_internal"`
	} `json:"conversations"`
}

// agyDir is agy's data directory.
func agyDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity-cli"), nil
}

// ListAgySessions returns agy conversations whose workspace is absPath, newest
// first, capped at limit (0 = all). agy keeps one index of its conversations
// with their workspace folders, so this is a single file read. agy indexes
// only interactive conversations, not headless -p runs, so this lists the
// sessions the owner had in a terminal; the bridge tracks its own runs' ids.
func ListAgySessions(absPath string, limit int) ([]Session, error) {
	dir, err := agyDir()
	if err != nil {
		return nil, err
	}
	return listAgySessionsFrom(filepath.Join(dir, "cache", "conversation_metadata.json"), absPath, limit)
}

func listAgySessionsFrom(path, absPath string, limit int) ([]Session, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta agyMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	want := filepath.Clean(absPath)
	var sessions []Session
	for id, c := range meta.Conversations {
		s := c.Summary
		if c.IsInternal || s.Internal || !agyInWorkspace(s.WorkspaceURIs, want) {
			continue
		}
		if s.ID != "" {
			id = s.ID
		}
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = strings.TrimSpace(s.Preview)
		}
		if title == "" {
			title = "(agy conversation)"
		}
		sessions = append(sessions, Session{ID: id, Title: snippet(title, 60), Modified: s.UpdatedAt})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Modified.After(sessions[j].Modified) })
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}

// agyInWorkspace reports whether any file:// workspace URI is absPath.
func agyInWorkspace(uris []string, absPath string) bool {
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Scheme != "file" {
			continue
		}
		if filepath.Clean(parsed.Path) == absPath {
			return true
		}
	}
	return false
}

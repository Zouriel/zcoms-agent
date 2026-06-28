package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ListSessionsFor lists past sessions for a project directory using the given
// backend's on-disk session store.
func ListSessionsFor(backend Backend, absPath string, limit int) ([]Session, error) {
	if backend.normalize() == BackendCodex {
		return ListCodexSessions(absPath, limit)
	}
	return ListSessions(absPath, limit)
}

// ListCodexSessions returns Codex sessions whose recorded working directory is
// absPath, newest first. Codex stores one rollout-*.jsonl per session under
// ~/.codex/sessions/YYYY/MM/DD/, beginning with a session_meta line.
func ListCodexSessions(absPath string, limit int) ([]Session, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".codex", "sessions")

	type fileInfo struct {
		path string
		mod  time.Time
	}
	var files []fileInfo
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, fileInfo{path, info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })

	titles := codexTitleIndex(home) // session id -> first prompt

	var sessions []Session
	for _, f := range files {
		if limit > 0 && len(sessions) >= limit {
			break
		}
		id, cwd := codexSessionMeta(f.path)
		if id == "" || cwd != absPath {
			continue
		}
		title := titles[id]
		if title == "" {
			title = "(codex session)"
		}
		sessions = append(sessions, Session{ID: id, Title: snippet(title, 60), Modified: f.mod})
	}
	return sessions, nil
}

// codexSessionMeta reads the first session_meta line for the id and cwd.
func codexSessionMeta(path string) (id, cwd string) {
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for i := 0; i < 5; i++ { // session_meta is the first line, but be lenient
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var entry struct {
				Type    string `json:"type"`
				Payload struct {
					ID  string `json:"id"`
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &entry) == nil && entry.Type == "session_meta" {
				return entry.Payload.ID, entry.Payload.Cwd
			}
		}
		if err != nil {
			break
		}
	}
	return "", ""
}

// codexTitleIndex maps each session id to its first user prompt, read once from
// ~/.codex/history.jsonl.
func codexTitleIndex(home string) map[string]string {
	titles := map[string]string{}
	file, err := os.Open(filepath.Join(home, ".codex", "history.jsonl"))
	if err != nil {
		return titles
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var entry struct {
				SessionID string `json:"session_id"`
				Text      string `json:"text"`
			}
			if json.Unmarshal(line, &entry) == nil && entry.SessionID != "" {
				if _, seen := titles[entry.SessionID]; !seen && entry.Text != "" {
					titles[entry.SessionID] = entry.Text
				}
			}
		}
		if err != nil {
			break
		}
	}
	return titles
}

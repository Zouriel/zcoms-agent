package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session is one Claude Code conversation belonging to a project directory.
type Session struct {
	ID       string
	Title    string
	Modified time.Time
}

// pathHash mirrors Claude Code's project-directory encoding: the absolute path
// with "/" and "." replaced by "-" (e.g. /home/u/work/HEMS -> -home-u-work-HEMS).
func pathHash(absPath string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(absPath)
}

func projectDir(absPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects", pathHash(absPath)), nil
}

// ListSessions returns the Claude Code sessions for a project directory, newest
// first, capped at limit (0 = all). Titles are read only for the returned set.
func ListSessions(absPath string, limit int) ([]Session, error) {
	dir, err := projectDir(absPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	type fileInfo struct {
		id  string
		mod time.Time
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{
			id:  strings.TrimSuffix(e.Name(), ".jsonl"),
			mod: info.ModTime(),
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}

	sessions := make([]Session, 0, len(files))
	for _, f := range files {
		sessions = append(sessions, Session{
			ID:       f.id,
			Title:    extractTitle(filepath.Join(dir, f.id+".jsonl")),
			Modified: f.mod,
		})
	}
	return sessions, nil
}

var (
	titleMarker  = []byte(`"type":"ai-title"`)
	userMarker   = []byte(`"type":"user"`)
	titleMarker2 = []byte(`"type": "ai-title"`)
	userMarker2  = []byte(`"type": "user"`)
)

// extractTitle returns the most recent ai-title for a session, falling back to a
// snippet of the first user prompt, then "(untitled session)". It only JSON-parses
// the handful of lines that could carry a title, so big transcripts stay cheap.
func extractTitle(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "(untitled session)"
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	title := ""
	firstUser := ""

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if bytes.Contains(line, titleMarker) || bytes.Contains(line, titleMarker2) {
				var t struct {
					Title string `json:"title"`
				}
				if json.Unmarshal(line, &t) == nil && t.Title != "" {
					title = t.Title
				}
			} else if firstUser == "" && (bytes.Contains(line, userMarker) || bytes.Contains(line, userMarker2)) {
				firstUser = firstUserText(line)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}

	if title != "" {
		return title
	}
	if firstUser != "" {
		return snippet(firstUser, 60)
	}
	return "(untitled session)"
}

func firstUserText(line []byte) string {
	var entry struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &entry) != nil {
		return ""
	}
	// content may be a plain string or an array of typed parts.
	var asString string
	if json.Unmarshal(entry.Message.Content, &asString) == nil {
		return asString
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(entry.Message.Content, &parts) == nil {
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

func snippet(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

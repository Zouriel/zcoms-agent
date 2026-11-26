package groups

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Limits on what a call may send back besides its text.
const (
	MaxAttachments     = 5        // files per call
	MaxAttachmentBytes = 20 << 20 // per file (the comms daemon enforces it too)
	MaxThreadName      = 64       // runes in a new thread's name
	MaxTopicsPerHour   = 5        // new threads per group per rolling hour
)

// Directives are the code-handled lines a work pass may add to its reply.
// They are requests: code decides what, if anything, happens.
type Directives struct {
	Attach []string // ATTACH: <path> lines, in order
	Thread string   // the first THREAD: <name> line
}

// SplitDirectives removes ATTACH: and THREAD: lines from a work-pass answer
// and returns the remaining reply text with them.
func SplitDirectives(out string) (string, Directives) {
	var d Directives
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			switch strings.ToUpper(strings.TrimSpace(k)) {
			case "ATTACH":
				if p := strings.Trim(strings.TrimSpace(v), "`\"'<>"); p != "" {
					d.Attach = append(d.Attach, p)
				}
				continue
			case "THREAD":
				if d.Thread == "" {
					d.Thread = cleanThreadName(v)
				}
				continue
			}
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n")), d
}

// cleanThreadName makes a model-proposed thread name safe to use as a topic
// title: one line, no markup quotes, capped.
func cleanThreadName(s string) string {
	s = strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(s), "`\"'<>")), " ")
	if strings.EqualFold(s, "none") {
		return ""
	}
	if r := []rune(s); len(r) > MaxThreadName {
		s = strings.TrimSpace(string(r[:MaxThreadName]))
	}
	return s
}

// secretFile reports whether a path looks like a secret: .env*, *.pem,
// *.key, credentials*, or anything under a secrets/ folder. Matches the
// project-group read denies.
func secretFile(p string) bool {
	p = filepath.ToSlash(p)
	base := strings.ToLower(filepath.Base(p))
	switch {
	case strings.HasPrefix(base, ".env"),
		strings.HasSuffix(base, ".pem"),
		strings.HasSuffix(base, ".key"),
		strings.HasPrefix(base, "credentials"):
		return true
	}
	for _, part := range strings.Split(strings.ToLower(p), "/") {
		if part == "secrets" {
			return true
		}
	}
	return false
}

// within reports whether p is root or inside it (both cleaned, absolute).
func within(p, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// Skipped is one ATTACH path that was not sent, with the reason.
type Skipped struct {
	Path, Reason string
}

// ValidateAttachments decides which ATTACH paths may be sent. A path must
// resolve (symlinks followed) to a regular file inside outbox or, for a
// project group, inside projectDir; be at most MaxAttachmentBytes; and not
// look like a secret, before or after resolving. A relative path is taken
// relative to the outbox. At most MaxAttachments are accepted.
func ValidateAttachments(paths []string, outbox, projectDir string) (ok []string, skipped []Skipped) {
	roots := make([]string, 0, 2)
	for _, r := range []string{outbox, projectDir} {
		if r == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(r); err == nil {
			roots = append(roots, filepath.Clean(real))
		}
	}
	seen := map[string]bool{}
	for _, p := range paths {
		reason := ""
		given := p
		if !filepath.IsAbs(p) {
			if outbox == "" {
				skipped = append(skipped, Skipped{given, "relative path with no outbox"})
				continue
			}
			p = filepath.Join(outbox, p)
		}
		p = filepath.Clean(p)
		real, err := filepath.EvalSymlinks(p)
		switch {
		case len(ok) >= MaxAttachments:
			reason = fmt.Sprintf("more than %d files", MaxAttachments)
		case secretFile(p):
			reason = "looks like a secret"
		case err != nil:
			reason = "not found"
		case secretFile(real):
			reason = "resolves to a secret"
		case !inAny(real, roots):
			reason = "outside the outbox and the project folder"
		}
		if reason == "" {
			info, err := os.Stat(real)
			switch {
			case err != nil:
				reason = "not readable"
			case !info.Mode().IsRegular():
				reason = "not a regular file"
			case info.Size() > MaxAttachmentBytes:
				reason = "over 20 MB"
			case seen[real]:
				reason = "listed twice"
			}
		}
		if reason != "" {
			skipped = append(skipped, Skipped{given, reason})
			continue
		}
		seen[real] = true
		ok = append(ok, real)
	}
	return ok, skipped
}

func inAny(p string, roots []string) bool {
	for _, r := range roots {
		if within(p, r) {
			return true
		}
	}
	return false
}

// topicLimiter counts new threads per group per rolling hour.
type topicLimiter struct {
	mu     sync.Mutex
	stamps map[string][]time.Time
}

func (l *topicLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stamps == nil {
		l.stamps = map[string][]time.Time{}
	}
	var kept []time.Time
	for _, t := range l.stamps[key] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= MaxTopicsPerHour {
		l.stamps[key] = kept
		return false
	}
	l.stamps[key] = append(kept, now)
	return true
}

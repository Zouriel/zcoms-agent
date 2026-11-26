package projects

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// Block markers around the managed section in a repo's agent instructions.
const (
	blockBegin = "<!-- zcoms:begin (managed by zc project init; edits inside are overwritten) -->"
	blockEnd   = "<!-- zcoms:end -->"
)

// instructionNames are the agent instruction files the block goes into,
// matched case-insensitively.
var instructionNames = map[string]bool{"agents.md": true, "agent.md": true, "claude.md": true}

// TransportLabel is the display name of a transport.
func TransportLabel(t string) string {
	if t == "whatsapp" {
		return "WhatsApp"
	}
	return "Telegram"
}

// BlockText renders the managed block for a group. Names and platform only:
// no ids, so the block cannot be used to repoint the binding. It tells coding
// agents to keep the main chat for milestones and to put screenshots, files
// and step-by-step progress in a thread.
func BlockText(transport, group string) string {
	lines := []string{
		blockBegin,
		"## zcoms project group",
		"",
		fmt.Sprintf("This repo is bound to the %s group %q.", TransportLabel(transport), group),
		"",
		"When the user asks for an update (\"update me on tele\", \"let me know when it's done\"),",
		"send it to the project group with zc project say, not privately.",
		"",
	}
	if transport == "whatsapp" {
		lines = append(lines,
			"WhatsApp groups have no threads, so keep group posts to important milestones:",
			"done, blocked, or a decision needed: zc project say \"<text>\".",
			"Send screenshots or files only when asked: zc project say --file <path> \"<caption>\".",
		)
	} else {
		lines = append(lines,
			"Screenshots, files and step-by-step progress go to a thread, not the main chat:",
			"zc project say --thread \"<short topic for the work>\" --file <path> \"<caption>\"",
			"(--file is optional and may repeat; photos are sent as photos, 20 MB each).",
			"Reuse the same thread name for the same piece of work; the command creates the",
			"thread if it does not exist.",
			"Post in the main chat (zc project say \"<text>\" without --thread) only for important",
			"milestones: done, blocked, or a decision needed.",
			"When you finish UI work, take a screenshot of the running app (for example",
			"npx playwright screenshot --full-page <url> /tmp/<name>.png) and post it to the thread.",
		)
	}
	lines = append(lines, "Do not message the user privately about this project.", blockEnd)
	return strings.Join(lines, "\n")
}

// UpsertBlock returns content with the managed block replaced (between the
// markers only) or appended when absent.
func UpsertBlock(content, block string) string {
	if i, j, ok := blockSpan(content); ok {
		return content[:i] + block + content[j:]
	}
	if content == "" {
		return block + "\n"
	}
	sep := "\n\n"
	if strings.HasSuffix(content, "\n\n") {
		sep = ""
	} else if strings.HasSuffix(content, "\n") {
		sep = "\n"
	}
	return content + sep + block + "\n"
}

// RemoveBlock returns content without the managed block (and the blank line
// that preceded it).
func RemoveBlock(content string) string {
	i, j, ok := blockSpan(content)
	if !ok {
		return content
	}
	before := strings.TrimRight(content[:i], "\n")
	after := strings.TrimLeft(content[j:], "\n")
	switch {
	case before == "":
		return after
	case after == "":
		return before + "\n"
	default:
		return before + "\n\n" + after
	}
}

// blockSpan finds the managed block: i is the start of the begin marker, j
// the end of the end marker.
func blockSpan(content string) (int, int, bool) {
	i := strings.Index(content, "<!-- zcoms:begin")
	if i < 0 {
		return 0, 0, false
	}
	k := strings.Index(content[i:], blockEnd)
	if k < 0 {
		return 0, 0, false
	}
	return i, i + k + len(blockEnd), true
}

// FindInstructionFiles lists AGENTS.md / AGENT.md / CLAUDE.md files at any
// depth under folder (case-insensitive), skipping .git, node_modules, vendor
// and anything git ignores.
func FindInstructionFiles(folder string) []string {
	var out []string
	_ = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != folder && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if instructionNames[strings.ToLower(d.Name())] && !gitIgnored(p) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// gitIgnored reports whether git ignores path (false outside a repo or when git
// is not installed).
func gitIgnored(path string) bool {
	if !gitAvailable() {
		return false
	}
	cmd := exec.Command("git", "-C", filepath.Dir(path), "check-ignore", "-q", path)
	return cmd.Run() == nil
}

// WriteBlocks puts the managed block into every instruction file under folder
// and returns the files it changed. When there are none and create is set, it
// writes AGENTS.md at the folder root holding only the block.
func WriteBlocks(folder, transport, group string, create bool) ([]string, error) {
	block := BlockText(transport, group)
	files := FindInstructionFiles(folder)
	if len(files) == 0 && create {
		files = []string{filepath.Join(folder, "AGENTS.md")}
	}
	var changed []string
	for _, f := range files {
		old, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			return changed, err
		}
		next := UpsertBlock(string(old), block)
		if next == string(old) {
			continue
		}
		if err := os.WriteFile(f, []byte(next), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, f)
	}
	return changed, nil
}

// StripBlocks removes the managed block from every instruction file under
// folder and returns the files it changed.
func StripBlocks(folder string) ([]string, error) {
	var changed []string
	for _, f := range FindInstructionFiles(folder) {
		old, err := os.ReadFile(f)
		if err != nil {
			return changed, err
		}
		next := RemoveBlock(string(old))
		if next == string(old) {
			continue
		}
		if err := os.WriteFile(f, []byte(next), 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, f)
	}
	return changed, nil
}

// SessionNote is the first-turn guidance for an owner's coding session in a
// bound folder: requested progress updates go to the project group, ordinary
// replies stay in the private chat.
func SessionNote(p store.Project) string {
	threads := "Put screenshots, files and step-by-step progress in a thread (`zc project say --thread \"<topic>\" --file <path> \"<caption>\"`) and keep the main chat for milestones. "
	if p.Transport == "whatsapp" {
		threads = "WhatsApp has no threads, so post only milestones, and screenshots only when asked (`zc project say --file <path> \"<caption>\"`). "
	}
	return fmt.Sprintf("This folder is bound to the %s group %q. When the owner asks you for a progress update "+
		"or to tell them when something is done, send that update to the group with `zc project say \"<text>\"` "+
		"instead of messaging them privately. "+threads+"Your ordinary replies in this chat stay here.",
		TransportLabel(p.Transport), p.GroupName)
}

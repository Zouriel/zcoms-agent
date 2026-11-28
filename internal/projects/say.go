package projects

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// MaxSayFileBytes caps each file `project say --file` sends (the comms
// daemon enforces the same 20 MB).
const MaxSayFileBytes = 20 << 20

// SayOpts is one `project say`: text, an optional thread (topic name or id)
// and files (absolute paths; photos go as photos). Pin pins a text update and
// keeps it from disappearing (Telegram).
type SayOpts struct {
	Text   string
	Thread string
	Files  []string
	Pin    bool
}

// say posts to the project's group. It runs only for owner commands; the
// target is the binding in agent.db, never anything in the text.
//
// With a thread, an existing topic of that name (or id) is reused, else one
// is created (the daemon turns topics on when the agent's account created
// the group). When the group cannot take a thread (WhatsApp, or topics off),
// text still goes to the main chat, but files go to the owner's private chat
// with a one-line note so the main chat is not flooded.
func (s *Service) say(p store.Project, o SayOpts) (string, error) {
	o.Text = strings.TrimSpace(o.Text)
	o.Thread = strings.TrimSpace(o.Thread)
	if o.Text == "" && len(o.Files) == 0 {
		return "", errors.New("usage: project say [--thread <name>] [--file <path>]... <text>")
	}
	for _, f := range o.Files {
		if err := checkSayFile(f); err != nil {
			return "", err
		}
	}
	// Files never go to the main chat: without a thread they go to the
	// project's updates thread.
	if o.Thread == "" && len(o.Files) > 0 && p.Transport != "whatsapp" {
		o.Thread = UpdatesThreadFor(p.Path)
	}
	thread, threadErr := "", error(nil)
	label, note := "", ""
	if o.Thread != "" {
		var t *client.Topic
		t, note, threadErr = s.topicFor(p, o.Thread)
		if threadErr == nil {
			thread, label = t.ID, t.Name
		}
	}
	if threadErr != nil {
		if len(o.Files) > 0 {
			return s.sayToOwner(p, o, threadErr)
		}
		pinNote, err := s.sendText(p, o, "")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Sent to the main chat of %q (no thread: %v). Keep main-chat posts to milestones.%s", p.GroupName, threadErr, pinNote), nil
	}

	where := fmt.Sprintf("%q", p.GroupName)
	if label != "" {
		where = fmt.Sprintf("the thread %q in %q", label, p.GroupName)
	}
	opts := client.SendOptions{Thread: thread}
	if len(o.Files) == 0 {
		pinNote, err := s.sendText(p, o, thread)
		if err != nil {
			return "", err
		}
		return "Sent to " + where + "." + note + pinNote, nil
	}
	if err := s.sendFiles(p.Transport, p.NativeID, o.Files, o.Text, opts); err != nil {
		return "", err
	}
	return fmt.Sprintf("Sent %d file(s) to %s.%s", len(o.Files), where, note), nil
}

// topicFor finds the group's thread named (or numbered) name. Coding
// sessions may not open new threads: only the project's updates thread is
// ever created; any other unknown name is posted to the updates thread, with
// a note saying so.
func (s *Service) topicFor(p store.Project, name string) (*client.Topic, string, error) {
	updates := UpdatesThreadFor(p.Path)
	ts, err := s.Owner.TopicList(p.Transport, p.NativeID)
	if err != nil && !topicsOffYet(err) {
		return nil, "", err
	}
	find := func(n string) *client.Topic {
		for _, t := range ts {
			if strings.EqualFold(strings.TrimSpace(t.Name), strings.TrimSpace(n)) || t.ID == n {
				t := t
				return &t
			}
		}
		return nil
	}
	if t := find(name); t != nil {
		return t, "", nil
	}
	note := ""
	if !strings.EqualFold(strings.TrimSpace(name), updates) {
		note = fmt.Sprintf(" (no thread %q, so it went to %q; coding sessions do not open new threads)", name, updates)
		if t := find(updates); t != nil {
			return t, note, nil
		}
	}
	t, err := s.Owner.TopicCreate(p.Transport, p.NativeID, updates)
	return t, note, err
}

// topicsOffYet reports whether a topic_list error only means the group is
// not a forum yet, which topic_create may still fix (it turns topics on in
// a group the agent's account created).
func topicsOffYet(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "topics are off")
}

// sayToOwner sends the files (caption on the first) to the owner's private
// chat with a one-line note, for a group that cannot take a thread.
func (s *Service) sayToOwner(p store.Project, o SayOpts, why error) (string, error) {
	owner := ""
	if s.OwnerChat != nil {
		owner = strings.TrimSpace(s.OwnerChat())
	}
	if owner == "" {
		return "", fmt.Errorf("no thread in %q (%v) and no owner chat to send the files to instead", p.GroupName, why)
	}
	note := fmt.Sprintf("These files for %q (%s) came here because the group could not take a thread: %v.", o.Thread, p.GroupName, why)
	if _, err := s.Owner.SendWith("telegram", owner, note, client.SendOptions{}); err != nil {
		return "", err
	}
	if err := s.sendFiles("telegram", owner, o.Files, o.Text, client.SendOptions{}); err != nil {
		return "", err
	}
	return fmt.Sprintf("No thread in %q (%v): sent %d file(s) to your private chat instead.", p.GroupName, why, len(o.Files)), nil
}

// sendFiles sends files one by one, the caption on the first.
func (s *Service) sendFiles(transport, to string, files []string, caption string, o client.SendOptions) error {
	for i, f := range files {
		c := ""
		if i == 0 {
			c = caption
		}
		if _, err := s.Owner.SendFileWith(transport, to, f, c, o); err != nil {
			return fmt.Errorf("sending %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func checkSayFile(f string) error {
	if !filepath.IsAbs(f) {
		return fmt.Errorf("%s: give an absolute path", f)
	}
	info, err := os.Stat(f)
	if err != nil {
		return fmt.Errorf("file not found: %s", f)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", f)
	}
	if info.Size() > MaxSayFileBytes {
		return fmt.Errorf("%s is over 20 MB", f)
	}
	return nil
}

// sendText posts a text update in thread ("" = main chat). With Pin on
// Telegram it is sent outside any disappearing timer and pinned; on WhatsApp,
// which has no pins, it is sent plainly and the returned note says so.
func (s *Service) sendText(p store.Project, o SayOpts, thread string) (string, error) {
	if o.Pin && p.Transport != "whatsapp" {
		if _, err := s.Owner.SendKeep(p.NativeID, o.Text, client.KeepOptions{Thread: thread, Pin: true, Silent: true}); err != nil {
			return "", err
		}
		return " Pinned; it will not disappear.", nil
	}
	if _, err := s.Owner.SendWith(p.Transport, p.NativeID, o.Text, client.SendOptions{Thread: thread}); err != nil {
		return "", err
	}
	if o.Pin {
		return " (Not pinned: WhatsApp has no pins here.)", nil
	}
	return "", nil
}

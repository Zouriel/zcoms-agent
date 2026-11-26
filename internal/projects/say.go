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
// and files (absolute paths; photos go as photos).
type SayOpts struct {
	Text   string
	Thread string
	Files  []string
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
	thread, threadErr := "", error(nil)
	label := ""
	if o.Thread != "" {
		var t *client.Topic
		t, threadErr = s.topicFor(p, o.Thread)
		if threadErr == nil {
			thread, label = t.ID, t.Name
		}
	}
	if threadErr != nil {
		if len(o.Files) > 0 {
			return s.sayToOwner(p, o, threadErr)
		}
		if _, err := s.Owner.SendWith(p.Transport, p.NativeID, o.Text, client.SendOptions{}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Sent to the main chat of %q (no thread: %v). Keep main-chat posts to milestones.", p.GroupName, threadErr), nil
	}

	where := fmt.Sprintf("%q", p.GroupName)
	if label != "" {
		where = fmt.Sprintf("the thread %q in %q", label, p.GroupName)
	}
	opts := client.SendOptions{Thread: thread}
	if len(o.Files) == 0 {
		if _, err := s.Owner.SendWith(p.Transport, p.NativeID, o.Text, opts); err != nil {
			return "", err
		}
		return "Sent to " + where + ".", nil
	}
	if err := s.sendFiles(p.Transport, p.NativeID, o.Files, o.Text, opts); err != nil {
		return "", err
	}
	return fmt.Sprintf("Sent %d file(s) to %s.", len(o.Files), where), nil
}

// topicFor finds the group's thread named (or numbered) name, creating it
// when there is none.
func (s *Service) topicFor(p store.Project, name string) (*client.Topic, error) {
	ts, err := s.Owner.TopicList(p.Transport, p.NativeID)
	if err == nil {
		for _, t := range ts {
			if strings.EqualFold(t.Name, name) || t.ID == name {
				t := t
				return &t, nil
			}
		}
	} else if !topicsOffYet(err) {
		return nil, err
	}
	return s.Owner.TopicCreate(p.Transport, p.NativeID, name)
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

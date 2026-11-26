package projects

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Indian/Maldives on hosts without a zoneinfo database

	"github.com/Zouriel/zcoms-agent/internal/cmdline"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// StandupPersona is the persona key the standup passes run on.
const StandupPersona = "project_standup"

// OwnerComms is the owner-caller comms client: only owner commands use it.
type OwnerComms interface {
	GroupGet(transport, group string) (*client.Group, error)
	GroupMembers(transport, group string) ([]client.GroupMember, error)
	GroupSet(transport, group string, listen, follow *bool) (*client.Group, error)
	SendOpts(transport, to, text, replyTo string, mentions []string) (client.Response, error)
	ListContacts() ([]client.Contact, error)
}

// AgentComms is the agent-caller comms client the standup uses; the daemon
// applies its agent limits (group_read by date only for follow groups).
type AgentComms interface {
	GroupRead(transport, group string, o client.GroupReadOpts) ([]client.GroupMessage, error)
	GroupMembers(transport, group string) ([]client.GroupMember, error)
	SendOpts(transport, to, text, replyTo string, mentions []string) (client.Response, error)
}

// Service runs the project commands, the standup and the project side of
// group calls.
type Service struct {
	Store *store.Store
	Owner OwnerComms
	Agent AgentComms
	// Run runs one tool-less model turn on a persona in dir.
	Run func(persona, dir, prompt string) (string, error)
	// OnFollow is told when a project's follow tasks may have changed, so the
	// caller can (re)register its standup job.
	OnFollow func(p store.Project)
	Log      *log.Logger
	Now      func() time.Time

	mu sync.Mutex // one project write path at a time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

// StandupZone is where the 09:00 standup is scheduled.
func StandupZone() *time.Location {
	if loc, err := time.LoadLocation("Indian/Maldives"); err == nil {
		return loc
	}
	return time.FixedZone("MVT", 5*3600)
}

func boolPtr(b bool) *bool { return &b }

// Command runs one `project …` command line (the words after "project").
// Callers guarantee it comes from the owner.
func (s *Service) Command(words []string) (string, error) {
	if len(words) == 0 {
		return "", errors.New("usage: project init|status|remove|members|alias|tasks|follow|standup|sync|say|probe --path <folder> …")
	}
	sub := words[0]
	a, err := cmdline.Parse(words[1:], "create-agents-md", "purge")
	if err != nil {
		return "", err
	}
	path := a.Flags["path"]
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("--path <absolute folder> is required")
	}
	path = filepath.Clean(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch sub {
	case "probe":
		return fmt.Sprintf("instructions=%d", len(FindInstructionFiles(path))), nil
	case "init":
		return s.initProject(path, a)
	case "status":
		return s.status(path)
	}
	p, ok, err := s.Store.ProjectForPath(path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("this folder is not inside a bound project; run zc project init in the project folder first")
	}
	switch sub {
	case "remove":
		return s.remove(p, a.Has("purge"))
	case "members":
		return s.members(p)
	case "alias":
		return s.alias(p, a.Pos)
	case "tasks", "task":
		return s.tasks(p, a.Pos)
	case "follow":
		return s.follow(p, a.Pos)
	case "standup":
		if len(a.Pos) == 0 || a.Pos[0] != "now" {
			return "", errors.New("usage: project standup now")
		}
		return s.Standup(p, true)
	case "sync":
		return s.sync(p)
	case "say":
		return s.say(p, strings.Join(a.Pos, " "))
	default:
		return "", fmt.Errorf("unknown project command %q", sub)
	}
}

func (s *Service) initProject(path string, a cmdline.Args) (string, error) {
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a folder", path)
	}
	all, err := s.Store.ListProjects()
	if err != nil {
		return "", err
	}
	for _, p := range all {
		if within(path, p.Path) {
			return "", fmt.Errorf("this folder is already bound: %s is the project for the group %q", p.Path, p.GroupName)
		}
		if within(p.Path, path) {
			return "", fmt.Errorf("%s inside this folder is already a bound project; remove it first", p.Path)
		}
	}
	tp := strings.ToLower(a.Flags["transport"])
	if tp == "" {
		tp = "telegram"
	}
	if tp != "telegram" && tp != "whatsapp" {
		return "", fmt.Errorf("unknown transport %q (want telegram or whatsapp)", tp)
	}
	name := strings.TrimSpace(a.Flags["group"])
	if name == "" {
		return "", errors.New("--group <name> is required")
	}
	follow, err := onOff(a.Flags["follow"], false)
	if err != nil {
		return "", err
	}
	g, err := s.Owner.GroupGet(tp, name)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", fmt.Errorf("no %s group called %q", tp, name)
	}
	if pol, ok, err := s.Store.GetGroupPolicy(tp, g.NativeID); err != nil {
		return "", err
	} else if ok && pol.Kind == store.GroupProject {
		return "", fmt.Errorf("the group %q is already bound to another project", g.Name)
	}

	var changed []string
	for _, repo := range ReposFor(path) {
		did, err := EnsureIgnored(repo, path)
		if err != nil {
			return "", fmt.Errorf("updating %s/.gitignore: %w", repo, err)
		}
		if did {
			changed = append(changed, filepath.Join(repo, ".gitignore"))
		}
	}
	if err := GuardUntracked(path); err != nil {
		return "", err
	}

	ws, ok, err := s.Store.GetWorkspaceByPath(path)
	if err != nil {
		return "", err
	}
	if !ok {
		if err := s.Store.UpsertDiscovered(path, filepath.Base(path)); err != nil {
			return "", err
		}
		if ws, _, err = s.Store.GetWorkspaceByPath(path); err != nil {
			return "", err
		}
	}

	h, err := OpenHistory(path)
	if err != nil {
		return "", err
	}
	defer h.Close()
	members, err := s.Owner.GroupMembers(tp, g.NativeID)
	if err != nil {
		return "", fmt.Errorf("reading the group's members: %w", err)
	}
	if _, _, err := h.SyncMembers(tp, toMembers(members)); err != nil {
		return "", err
	}

	dot, _, err := ReadDotfile(path)
	if err != nil {
		return "", err
	}
	dot.Group, dot.FollowTasks = g.Name, follow
	for _, m := range toMembers(members) {
		dot.ensureMember(m.Name)
	}
	s.copyContactAliases(&dot)
	if err := WriteDotfile(path, dot); err != nil {
		return "", err
	}
	changed = append(changed, filepath.Join(path, DotfileName))

	p, err := s.Store.CreateProject(store.Owner, store.Project{
		WorkspaceID: ws.ID, Transport: tp, NativeID: g.NativeID, GroupName: g.Name, FollowTasks: follow,
	})
	if err != nil {
		return "", err
	}
	p.Path, p.MaxRole = ws.Path, ws.MaxRole
	if _, err := s.Owner.GroupSet(tp, g.NativeID, boolPtr(true), boolPtr(follow)); err != nil {
		s.logf("project init: group_set: %v", err)
		changed = append(changed, "(warning: could not turn listening on in comms: "+err.Error()+")")
	}
	files, err := WriteBlocks(path, tp, g.Name, a.Has("create-agents-md"))
	if err != nil {
		return "", err
	}
	changed = append(changed, files...)
	if s.OnFollow != nil {
		s.OnFollow(p)
	}
	return fmt.Sprintf("Bound %s to the %s group %q. Follow tasks: %s.\nChanged:\n  %s",
		path, TransportLabel(tp), g.Name, onOffWord(follow), strings.Join(changed, "\n  ")), nil
}

// copyContactAliases copies aliases saved on a contact whose name matches a
// member into the .zcoms entry.
func (s *Service) copyContactAliases(d *Dotfile) {
	cs, err := s.Owner.ListContacts()
	if err != nil {
		return
	}
	for _, c := range cs {
		if m := d.member(c.Name); m != nil {
			for _, al := range c.Aliases {
				_ = d.AddAlias(m.Name, al)
			}
		}
	}
}

func toMembers(gm []client.GroupMember) []Member {
	out := make([]Member, 0, len(gm))
	for _, m := range gm {
		if m.IsSelf || m.ID == "" {
			continue
		}
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = m.Username
		}
		out = append(out, Member{Name: name, PlatformID: m.ID, Active: true})
	}
	return out
}

func (s *Service) status(path string) (string, error) {
	p, ok, err := s.Store.ProjectForPath(path)
	if err != nil {
		return "", err
	}
	if !ok {
		// A .zcoms here or above, but no binding: another clone or worktree.
		if dir := findDotfileUp(path); dir != "" {
			d, _, _ := ReadDotfile(dir)
			msg := fmt.Sprintf("%s has a .zcoms file but is not a bound project (it may be another clone or worktree).", dir)
			if all, err := s.Store.ListProjects(); err == nil {
				for _, q := range all {
					if strings.EqualFold(q.GroupName, d.Group) {
						msg += fmt.Sprintf(" The bound folder for %q is %s.", q.GroupName, q.Path)
					}
				}
			}
			return msg, nil
		}
		return "This folder is not inside a bound project.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\nGroup: %s (%s)\nFollow tasks: %s\n", p.Path, p.GroupName, TransportLabel(p.Transport), onOffWord(p.FollowTasks))
	if p.MaxRole == "read" {
		b.WriteString("Shell: withheld (workspace max_role is read)\n")
	}
	if d, ok, err := ReadDotfile(p.Path); err != nil {
		fmt.Fprintf(&b, "Warning: %v\n", err)
	} else if !ok {
		b.WriteString("Warning: .zcoms is missing; run zc project sync\n")
	} else if !strings.EqualFold(strings.TrimSpace(d.Group), p.GroupName) {
		fmt.Fprintf(&b, "Warning: .zcoms names the group %q but the folder is bound to %q (the binding wins)\n", d.Group, p.GroupName)
	}
	if err := GuardUntracked(p.Path); err != nil {
		fmt.Fprintf(&b, "Warning: %v\n", err)
	}
	if h, err := OpenHistory(p.Path); err == nil {
		if ts, err := h.OpenTasks(); err == nil {
			fmt.Fprintf(&b, "Open tasks: %d\n", len(ts))
		}
		h.Close()
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func findDotfileUp(path string) string {
	for dir := path; ; {
		if _, err := os.Stat(filepath.Join(dir, DotfileName)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func (s *Service) remove(p store.Project, purge bool) (string, error) {
	if err := s.Store.DeleteProject(store.Owner, p.ID); err != nil {
		return "", err
	}
	var notes []string
	if _, err := s.Owner.GroupSet(p.Transport, p.NativeID, boolPtr(false), boolPtr(false)); err != nil {
		notes = append(notes, "warning: could not turn listening off in comms: "+err.Error())
	}
	files, err := StripBlocks(p.Path)
	if err != nil {
		notes = append(notes, "warning: "+err.Error())
	}
	notes = append(notes, files...)
	if purge {
		if err := os.RemoveAll(filepath.Join(p.Path, HistoryDir)); err != nil {
			notes = append(notes, "warning: "+err.Error())
		}
		if err := os.Remove(filepath.Join(p.Path, DotfileName)); err != nil && !os.IsNotExist(err) {
			notes = append(notes, "warning: "+err.Error())
		}
		notes = append(notes, "deleted "+filepath.Join(p.Path, HistoryDir)+" and "+DotfileName)
	}
	if s.OnFollow != nil {
		p.FollowTasks = false
		s.OnFollow(p)
	}
	msg := fmt.Sprintf("Unbound %s from %q.", p.Path, p.GroupName)
	if !purge {
		msg += " History and .zcoms are kept (use --purge to delete them)."
	}
	if len(notes) > 0 {
		msg += "\n  " + strings.Join(notes, "\n  ")
	}
	return msg, nil
}

func (s *Service) members(p store.Project) (string, error) {
	h, err := OpenHistory(p.Path)
	if err != nil {
		return "", err
	}
	defer h.Close()
	ms, err := h.Members()
	if err != nil {
		return "", err
	}
	dot, _, _ := ReadDotfile(p.Path)
	var b strings.Builder
	for _, m := range ms {
		state := ""
		if !m.Active {
			state = " (left)"
		}
		al := ""
		if dm := dot.member(m.Name); dm != nil && len(dm.Aliases) > 0 {
			al = "  aka " + strings.Join(dm.Aliases, ", ")
		}
		fmt.Fprintf(&b, "%s%s%s\n", m.Name, state, al)
	}
	if b.Len() == 0 {
		return "No members recorded yet.", nil
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (s *Service) alias(p store.Project, pos []string) (string, error) {
	if len(pos) < 3 || (pos[0] != "add" && pos[0] != "rm") {
		return "", errors.New("usage: project alias add|rm <member> <alias>")
	}
	dot, ok, err := ReadDotfile(p.Path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New(".zcoms is missing; run zc project sync")
	}
	member, alias := pos[1], strings.Join(pos[2:], " ")
	if pos[0] == "add" {
		err = dot.AddAlias(member, alias)
	} else {
		err = dot.RemoveAlias(member, alias)
	}
	if err != nil {
		return "", err
	}
	if err := WriteDotfile(p.Path, dot); err != nil {
		return "", err
	}
	return "Saved in " + filepath.Join(p.Path, DotfileName) + ".", nil
}

func (s *Service) tasks(p store.Project, pos []string) (string, error) {
	if err := GuardUntracked(p.Path); err != nil {
		return "", err
	}
	h, err := OpenHistory(p.Path)
	if err != nil {
		return "", err
	}
	defer h.Close()
	if len(pos) == 0 || pos[0] == "list" {
		ts, err := h.OpenTasks()
		if err != nil {
			return "", err
		}
		if len(ts) == 0 {
			return "No open tasks.", nil
		}
		var b strings.Builder
		for _, t := range ts {
			who := t.AssigneeName
			if who == "" {
				who = "unassigned"
			}
			fmt.Fprintf(&b, "#%d %s (%s)\n", t.ID, t.Title, who)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	if len(pos) < 2 {
		return "", errors.New("usage: project tasks [done|drop <id> | assign <id> <member>]")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(pos[1], "#"), 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad task id %q", pos[1])
	}
	t, ok, err := h.Task(id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no task #%d", id)
	}
	switch pos[0] {
	case "done":
		return fmt.Sprintf("Closed #%d.", id), h.SetTaskStatus(t.ID, TaskDone, "owner")
	case "drop":
		return fmt.Sprintf("Dropped #%d.", id), h.SetTaskStatus(t.ID, TaskDropped, "owner")
	case "assign":
		if len(pos) < 3 {
			return "", errors.New("usage: project tasks assign <id> <member>")
		}
		dot, _, _ := ReadDotfile(p.Path)
		who := strings.Join(pos[2:], " ")
		name, ok := dot.MatchMember(who)
		if !ok {
			name = who
		}
		m, ok, err := h.MemberByName(name)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("no active member called %q", who)
		}
		return fmt.Sprintf("Assigned #%d to %s.", id, m.Name), h.AssignTask(t.ID, m.ID, "owner")
	default:
		return "", fmt.Errorf("unknown tasks command %q", pos[0])
	}
}

func (s *Service) follow(p store.Project, pos []string) (string, error) {
	if len(pos) == 0 {
		return "", errors.New("usage: project follow on|off")
	}
	on, err := onOff(pos[0], false)
	if err != nil {
		return "", err
	}
	if err := s.Store.SetProjectFollow(store.Owner, p.ID, on); err != nil {
		return "", err
	}
	if _, err := s.Owner.GroupSet(p.Transport, p.NativeID, nil, boolPtr(on)); err != nil {
		return "", fmt.Errorf("saved, but could not change follow in comms: %w", err)
	}
	if dot, ok, err := ReadDotfile(p.Path); err == nil && ok {
		dot.FollowTasks = on
		_ = WriteDotfile(p.Path, dot)
	}
	p.FollowTasks = on
	if s.OnFollow != nil {
		s.OnFollow(p)
	}
	return "Follow tasks " + onOffWord(on) + ".", nil
}

func (s *Service) sync(p store.Project) (string, error) {
	files, err := WriteBlocks(p.Path, p.Transport, p.GroupName, false)
	if err != nil {
		return "", err
	}
	dot, _, err := ReadDotfile(p.Path)
	if err != nil {
		return "", err
	}
	dot.Group, dot.FollowTasks = p.GroupName, p.FollowTasks
	if ms, err := s.Owner.GroupMembers(p.Transport, p.NativeID); err == nil {
		if err := GuardUntracked(p.Path); err != nil {
			return "", err
		}
		if h, err := OpenHistory(p.Path); err == nil {
			_, _, _ = h.SyncMembers(p.Transport, toMembers(ms))
			h.Close()
		}
		for _, m := range toMembers(ms) {
			dot.ensureMember(m.Name)
		}
	}
	if err := WriteDotfile(p.Path, dot); err != nil {
		return "", err
	}
	files = append(files, filepath.Join(p.Path, DotfileName))
	return "Synced.\n  " + strings.Join(files, "\n  "), nil
}

func (s *Service) say(p store.Project, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("usage: project say <text>")
	}
	if _, err := s.Owner.SendOpts(p.Transport, p.NativeID, text, "", nil); err != nil {
		return "", err
	}
	return fmt.Sprintf("Sent to %q.", p.GroupName), nil
}

func onOff(v string, def bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return def, nil
	case "on", "yes", "y", "true":
		return true, nil
	case "off", "no", "n", "false":
		return false, nil
	}
	return false, fmt.Errorf("want on or off, got %q", v)
}

func onOffWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// DetailMember is a member with the nicknames .zcoms gives them.
type DetailMember struct {
	Member
	Aliases []string `json:"aliases,omitempty"`
}

// Detail is one project's console view: members, open tasks and the latest
// summaries.
type Detail struct {
	Project   store.Project  `json:"project"`
	Members   []DetailMember `json:"members"`
	OpenTasks []Task         `json:"open_tasks"`
	Closed    []Task         `json:"closed_tasks"` // done and dropped, newest 200
	Summaries []Summary      `json:"summaries"`
}

// Detail reads a project's console view. It only reads: a project whose
// history database does not exist yet returns the binding alone.
func (s *Service) Detail(id int64) (Detail, error) {
	p, ok, err := s.Store.GetProject(id)
	if err != nil {
		return Detail{}, err
	}
	if !ok {
		return Detail{}, fmt.Errorf("no project %d", id)
	}
	d := Detail{Project: p}
	h, err := openExisting(p.Path)
	if err != nil {
		return d, nil
	}
	defer h.Close()
	dot, _, _ := ReadDotfile(p.Path)
	ms, err := h.Members()
	if err != nil {
		return d, err
	}
	for _, m := range ms {
		dm := DetailMember{Member: m}
		if e := dot.member(m.Name); e != nil {
			dm.Aliases = e.Aliases
		}
		d.Members = append(d.Members, dm)
	}
	if d.OpenTasks, err = h.OpenTasks(); err != nil {
		return d, err
	}
	if d.Closed, err = h.ClosedTasks(200); err != nil {
		return d, err
	}
	d.Summaries, err = h.Summaries(30)
	return d, err
}

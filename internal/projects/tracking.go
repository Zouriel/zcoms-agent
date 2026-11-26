package projects

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/store"
)

// Task tracking for groups that are not repos. `group track on` gives a chat a
// folder under groups_dir with the same .zcoms and .zcoms.d/history.db a
// project gets, binds it as a tracked project with follow tasks on, and starts
// the morning standup. Whether a call in such a group gets project tools is
// decided per call: only once the folder holds a git repo.

// GroupsDirKey is the agent setting naming the base folder for tracked
// groups. Unset means DefaultGroupsDir.
const GroupsDirKey = "groups_dir"

// DefaultGroupsDir is the base folder for tracked groups.
const DefaultGroupsDir = "~/zcoms-groups"

// Display kinds of a group, as GroupPolicies and the console show them.
const (
	KindGeneral = "general"
	KindTasks   = "tasks"
	KindProject = "project"
)

// repoSearchDepth bounds the per-call walk looking for a repo in a tracked
// folder: the folder itself plus this many levels below it.
const repoSearchDepth = 3

// GroupsDir returns the base folder for tracked groups, with ~ expanded.
func GroupsDir(st *store.Store) string {
	dir := DefaultGroupsDir
	if st != nil {
		if v, err := st.GetSetting(GroupsDirKey); err == nil && strings.TrimSpace(v) != "" {
			dir = strings.TrimSpace(v)
		}
	}
	return expandHome(dir)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return filepath.Clean(p)
}

// Slug turns a group name into a filesystem-safe folder name: lower-case
// ASCII letters and digits joined by single dashes, at most 48 characters.
// A name with nothing usable becomes "group".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	if s == "" {
		s = "group"
	}
	return s
}

// UniqueFolder returns base/slug, or base/slug-2, -3 … , the first that does
// not exist on disk and is not taken (taken reports another group's folder).
func UniqueFolder(base, slug string, taken func(string) bool) string {
	for i := 1; ; i++ {
		name := slug
		if i > 1 {
			name = slug + "-" + strconv.Itoa(i)
		}
		p := filepath.Join(base, name)
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if taken != nil && taken(p) {
			continue
		}
		return p
	}
}

// RepoDirs returns the git repos in folder: folder itself when it is one, and
// any below it up to repoSearchDepth levels, skipping dependency folders and
// hidden folders. It is cheap enough to run on every call.
func RepoDirs(folder string) []string {
	var out []string
	_ = filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != folder {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if rel, err := filepath.Rel(folder, p); err == nil && strings.Count(rel, string(filepath.Separator)) >= repoSearchDepth {
				return filepath.SkipDir
			}
		}
		if _, serr := os.Stat(filepath.Join(p, ".git")); serr == nil {
			out = append(out, p)
			if p != folder {
				return filepath.SkipDir // a repo's own subfolders are its business
			}
		}
		return nil
	})
	return out
}

// HasRepo reports whether folder holds a git repo (itself or below).
func HasRepo(folder string) bool { return len(RepoDirs(folder)) > 0 }

// KindOf is the display kind of a bound project: a tracked folder with no
// repo is "tasks", anything else "project".
func KindOf(p store.Project) string {
	if p.Tracked && !HasRepo(p.Path) {
		return KindTasks
	}
	return KindProject
}

// roleRank orders the max_role ladder; "" (no cap) ranks highest.
func roleRank(r string) int {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "read":
		return 1
	case "confirm":
		return 2
	case "edit":
		return 3
	case "full":
		return 4
	}
	return 5
}

// TighterRole returns the more restrictive of two max_role values.
func TighterRole(a, b string) string {
	if roleRank(b) < roleRank(a) {
		return b
	}
	return a
}

// trackedRole registers the repos found in a tracked folder as workspaces
// when discovery has not, and returns the tightest max_role among the folder
// and those repos.
func (s *Service) trackedRole(p store.Project, repos []string) string {
	role := p.MaxRole
	for _, r := range repos {
		r = filepath.Clean(r)
		if r == filepath.Clean(p.Path) {
			continue
		}
		ws, ok, err := s.Store.GetWorkspaceByPath(r)
		if err != nil {
			continue
		}
		if !ok {
			if err := s.Store.UpsertDiscovered(r, filepath.Base(r)); err != nil {
				s.logf("registering %s: %v", r, err)
			}
			continue
		}
		role = TighterRole(role, ws.MaxRole)
	}
	return role
}

// Track runs `group track on|off <transport> <group> [--purge]`. Owner only:
// callers guarantee the line came from agent.sock.
func (s *Service) Track(tp, group string, on, purge bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.Owner.GroupGet(tp, group)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", fmt.Errorf("no %s group called %q", tp, group)
	}
	if on {
		return s.trackOn(tp, g.NativeID, g.Name)
	}
	p, ok, err := s.Store.ProjectForGroup(tp, g.NativeID)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("Task tracking is already off for %s.", g.Name), nil
	}
	if !p.Tracked {
		return "", fmt.Errorf("%s is a project group bound with zc project init; use zc project remove in %s", g.Name, p.Path)
	}
	return s.untrack(p, purge)
}

func (s *Service) trackOn(tp, nativeID, name string) (string, error) {
	pol, has, err := s.Store.GetGroupPolicy(tp, nativeID)
	if err != nil {
		return "", err
	}
	if has && pol.Kind == store.GroupProject {
		if p, ok, _ := s.Store.ProjectForGroup(tp, nativeID); ok && p.Tracked {
			return fmt.Sprintf("Task tracking is already on for %s (%s).", name, p.Path), nil
		}
		return "", fmt.Errorf("%s is already bound to a project folder", name)
	}
	restore := has && pol.Kind == store.GroupGeneral

	folder, known, err := s.Store.GroupFolder(tp, nativeID)
	if err != nil {
		return "", err
	}
	if !known {
		base := GroupsDir(s.Store)
		folder = UniqueFolder(base, Slug(name), func(p string) bool {
			t, err := s.Store.GroupFolderTaken(p)
			return err != nil || t
		})
	}
	all, err := s.Store.ListProjects()
	if err != nil {
		return "", err
	}
	for _, p := range all {
		if within(folder, p.Path) || within(p.Path, folder) {
			return "", fmt.Errorf("%s overlaps the bound project %s", folder, p.Path)
		}
	}
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", folder, err)
	}
	if !known {
		if err := s.Store.SetGroupFolder(store.Owner, tp, nativeID, folder); err != nil {
			return "", err
		}
	}
	for _, repo := range ReposFor(folder) {
		if _, err := EnsureIgnored(repo, folder); err != nil {
			return "", fmt.Errorf("updating %s/.gitignore: %w", repo, err)
		}
	}
	if err := GuardUntracked(folder); err != nil {
		return "", err
	}

	ws, ok, err := s.Store.GetWorkspaceByPath(folder)
	if err != nil {
		return "", err
	}
	if !ok {
		if err := s.Store.UpsertDiscovered(folder, filepath.Base(folder)); err != nil {
			return "", err
		}
		if ws, _, err = s.Store.GetWorkspaceByPath(folder); err != nil {
			return "", err
		}
	}

	h, err := OpenHistory(folder)
	if err != nil {
		return "", err
	}
	defer h.Close()
	dot, _, err := ReadDotfile(folder)
	if err != nil {
		return "", err
	}
	dot.Group, dot.FollowTasks = name, true
	var notes []string
	if members, err := s.Owner.GroupMembers(tp, nativeID); err != nil {
		notes = append(notes, "warning: could not read the members yet ("+err.Error()+"); the first standup will")
	} else {
		if _, _, err := h.SyncMembers(tp, toMembers(members)); err != nil {
			return "", err
		}
		for _, m := range toMembers(members) {
			dot.ensureMember(m.Name)
		}
	}
	s.copyContactAliases(&dot)
	if err := WriteDotfile(folder, dot); err != nil {
		return "", err
	}

	p, err := s.Store.CreateProject(store.Owner, store.Project{
		WorkspaceID: ws.ID, Transport: tp, NativeID: nativeID, GroupName: name,
		FollowTasks: true, Tracked: true, RestoreGeneral: restore,
	})
	if err != nil {
		return "", err
	}
	p.Path, p.MaxRole = ws.Path, ws.MaxRole
	if _, err := s.Owner.GroupSet(tp, nativeID, boolPtr(true), boolPtr(true)); err != nil {
		notes = append(notes, "warning: could not turn listening on in comms: "+err.Error())
	}
	if s.OnFollow != nil {
		s.OnFollow(p)
	}
	msg := fmt.Sprintf("Tracking tasks in %s. Folder: %s. The first standup is at the next 09:00.", name, folder)
	if len(notes) > 0 {
		msg += "\n  " + strings.Join(notes, "\n  ")
	}
	return msg, nil
}

// untrack unbinds a tracked group. The group goes back to general when it was
// general before tracking, else off. The folder and its data stay unless
// purge; a purge never deletes anything the owner put in the folder.
func (s *Service) untrack(p store.Project, purge bool) (string, error) {
	if err := s.Store.DeleteProject(store.Owner, p.ID); err != nil {
		return "", err
	}
	var notes []string
	listen := p.RestoreGeneral
	if listen {
		if err := s.Store.SetGroupPolicy(store.Owner, store.GroupPolicy{
			Transport: p.Transport, NativeID: p.NativeID, Name: p.GroupName, Kind: store.GroupGeneral,
		}); err != nil {
			notes = append(notes, "warning: could not turn the group back to general: "+err.Error())
			listen = false
		}
	}
	if _, err := s.Owner.GroupSet(p.Transport, p.NativeID, boolPtr(listen), boolPtr(false)); err != nil {
		notes = append(notes, "warning: could not update listening in comms: "+err.Error())
	}
	if files, err := StripBlocks(p.Path); err != nil {
		notes = append(notes, "warning: "+err.Error())
	} else {
		notes = append(notes, files...)
	}
	if purge {
		if err := os.RemoveAll(filepath.Join(p.Path, HistoryDir)); err != nil {
			notes = append(notes, "warning: "+err.Error())
		}
		if err := os.Remove(filepath.Join(p.Path, DotfileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			notes = append(notes, "warning: "+err.Error())
		}
		// The folder itself goes only when nothing else is in it.
		if err := os.Remove(p.Path); err == nil {
			_ = s.Store.DeleteGroupFolder(store.Owner, p.Transport, p.NativeID)
			notes = append(notes, "deleted "+p.Path)
		} else {
			notes = append(notes, "deleted the history and "+DotfileName+"; kept "+p.Path+" because it holds other files")
		}
	}
	if s.OnFollow != nil {
		p.FollowTasks = false
		s.OnFollow(p)
	}
	state := "off"
	if listen {
		state = "a general group again"
	}
	msg := fmt.Sprintf("Stopped tracking tasks in %s; it is %s.", p.GroupName, state)
	if !purge {
		msg += " The folder and history are kept in " + p.Path + "."
	}
	if len(notes) > 0 {
		msg += "\n  " + strings.Join(notes, "\n  ")
	}
	return msg, nil
}

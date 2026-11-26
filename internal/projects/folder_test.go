package projects

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

var acmeGroup = client.Group{Name: "Acme", Transport: "telegram", NativeID: "-1001"}

// trackedAcme returns a service with Acme tracked (a tasks group) and its
// project.
func trackedAcme(t *testing.T, groups ...client.Group) (*Service, *fakeOwner, store.Project, *[]store.Project) {
	t.Helper()
	s, fo, _, follows := trackService(t, append([]client.Group{acmeGroup}, groups...)...)
	if _, err := s.Track("telegram", "Acme", true, false); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.Store.ProjectForGroup("telegram", "-1001")
	return s, fo, p, follows
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestDirMovesHistoryWithWAL(t *testing.T) {
	s, _, p, follows := trackedAcme(t)
	old := p.Path

	// Keep a handle open so the WAL and shared-memory files are on disk.
	h, err := OpenHistory(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddTask("Book the venue", 0, ""); err != nil {
		t.Fatal(err)
	}
	if !exists(HistoryPath(old) + "-wal") {
		t.Fatal("test setup: no WAL file")
	}

	// The new folder holds a repo with an instruction file, so the tasks
	// group becomes a project group there.
	dest := filepath.Join(t.TempDir(), "acme")
	if err := os.MkdirAll(filepath.Join(dest, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "CLAUDE.md"), []byte("# Acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	*follows = nil
	msg, err := s.Dir("telegram", "Acme", dest, false)
	h.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "project now") {
		t.Fatalf("reply = %q", msg)
	}
	if exists(filepath.Join(old, HistoryDir)) || exists(filepath.Join(old, DotfileName)) {
		t.Fatal("old folder still has the history or .zcoms")
	}
	if !exists(filepath.Join(dest, DotfileName)) {
		t.Fatal(".zcoms not moved")
	}
	nh, err := openExisting(dest)
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := nh.OpenTasks()
	nh.Close()
	if len(ts) != 1 || ts[0].Title != "Book the venue" {
		t.Fatalf("tasks after the move = %+v", ts)
	}

	q, _, _ := s.Store.ProjectForGroup("telegram", "-1001")
	if q.ID != p.ID || q.Path != dest || !q.Tracked || !q.FollowTasks {
		t.Fatalf("binding = %+v", q)
	}
	if KindOf(q) != KindProject {
		t.Fatalf("kind = %s, want project", KindOf(q))
	}
	if f, _, _ := s.Store.GroupFolder("telegram", "-1001"); f != dest {
		t.Fatalf("group folder = %s", f)
	}
	if _, ok, _ := s.Store.GetWorkspaceByPath(dest); !ok {
		t.Fatal("new folder not registered as a workspace")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, ".gitignore")); !strings.Contains(string(b), ".zcoms.d/") {
		t.Fatalf(".gitignore = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "CLAUDE.md")); !strings.Contains(string(b), "zcoms:begin") {
		t.Fatalf("CLAUDE.md has no block: %q", b)
	}
	if len(*follows) != 1 || (*follows)[0].Path != dest {
		t.Fatalf("standup not rescheduled: %+v", *follows)
	}
}

func TestDirMovesBlockOutOfOldFolder(t *testing.T) {
	s, _, p, _ := trackedAcme(t)
	agents := filepath.Join(p.Path, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# Old\n\n"+BlockText("telegram", "Acme")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := s.Dir("telegram", "-1001", dest, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(agents); strings.Contains(string(b), "zcoms:begin") {
		t.Fatalf("old block left: %q", b)
	}
	if exists(filepath.Join(dest, "AGENTS.md")) {
		t.Fatal("created an instruction file")
	}
	q, _, _ := s.Store.ProjectForGroup("telegram", "-1001")
	if KindOf(q) != KindTasks {
		t.Fatalf("kind = %s, want tasks", KindOf(q))
	}
}

func TestDirCopiesAcrossFilesystems(t *testing.T) {
	s, _, p, _ := trackedAcme(t)
	h, _ := OpenHistory(p.Path)
	_, _ = h.AddTask("Cross", 0, "")
	h.Close()
	s.rename = func(a, b string) error { return &os.LinkError{Op: "rename", Old: a, New: b, Err: syscall.EXDEV} }
	dest := filepath.Join(t.TempDir(), "new", "acme")
	if _, err := s.Dir("telegram", "Acme", dest, true); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(p.Path, HistoryDir)) || exists(filepath.Join(p.Path, DotfileName)) {
		t.Fatal("source kept after the copy")
	}
	nh, err := openExisting(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer nh.Close()
	if ts, _ := nh.OpenTasks(); len(ts) != 1 || ts[0].Title != "Cross" {
		t.Fatalf("tasks = %+v", ts)
	}
}

func TestDirRollsBackMove(t *testing.T) {
	for _, step := range []string{"move", "commit"} {
		t.Run(step, func(t *testing.T) {
			s, _, p, follows := trackedAcme(t)
			parent := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(filepath.Join(parent, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(parent, "sub", "acme")
			s.failAt = func(name string) error {
				if name == step {
					return errors.New("boom")
				}
				return nil
			}
			*follows = nil
			_, err := s.Dir("telegram", "Acme", dest, true)
			if err == nil || !strings.Contains(err.Error(), "Rolled back") {
				t.Fatalf("err = %v", err)
			}
			for _, f := range []string{DotfileName, filepath.Join(HistoryDir, "history.db")} {
				if !exists(filepath.Join(p.Path, f)) {
					t.Fatalf("old folder lost %s", f)
				}
			}
			if exists(filepath.Join(parent, "sub")) {
				t.Fatal("created folders left behind")
			}
			if exists(filepath.Join(parent, ".gitignore")) {
				t.Fatal(".gitignore written by the failed move left behind")
			}
			q, _, _ := s.Store.ProjectForGroup("telegram", "-1001")
			if q.Path != p.Path {
				t.Fatalf("binding moved: %s", q.Path)
			}
			if f, _, _ := s.Store.GroupFolder("telegram", "-1001"); f != p.Path {
				t.Fatalf("group folder = %s", f)
			}
			if len(*follows) != 0 {
				t.Fatal("standup rescheduled after a failed move")
			}
			// The old folder still works.
			if _, err := s.Detail(p.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDirAddsFolderToGeneralGroup(t *testing.T) {
	s, fo, _, follows := trackService(t, acmeGroup)
	if err := s.Store.SetGroupPolicy(store.Owner, store.GroupPolicy{Transport: "telegram", NativeID: "-1001", Name: "Acme", Kind: store.GroupGeneral}); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "CLAUDE.md"), []byte("# Notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Dir("telegram", "Acme", dest, false); err != nil {
		t.Fatal(err)
	}
	p, ok, _ := s.Store.ProjectForGroup("telegram", "-1001")
	if !ok || p.Path != dest || p.Tracked || p.FollowTasks || !p.RestoreGeneral {
		t.Fatalf("binding = %+v %v", p, ok)
	}
	if pol, _, _ := s.Store.GetGroupPolicy("telegram", "-1001"); pol.Kind != store.GroupProject || pol.ProjectID != p.ID {
		t.Fatalf("policy = %+v", pol)
	}
	if KindOf(p) != KindProject {
		t.Fatalf("kind = %s", KindOf(p))
	}
	if !exists(HistoryPath(dest)) || !exists(filepath.Join(dest, DotfileName)) {
		t.Fatal("history or .zcoms missing")
	}
	if dot, _, _ := ReadDotfile(dest); dot.Group != "Acme" || dot.FollowTasks || dot.member("Ibrahim") == nil {
		t.Fatalf(".zcoms = %+v", dot)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "CLAUDE.md")); !strings.Contains(string(b), "zcoms:begin") {
		t.Fatal("no block in CLAUDE.md")
	}
	if exists(filepath.Join(dest, "AGENTS.md")) {
		t.Fatal("created AGENTS.md")
	}
	if c := fo.last(); !*c.listen || *c.follow {
		t.Fatalf("group_set = listen %v follow %v", *c.listen, *c.follow)
	}
	if len(*follows) != 1 || (*follows)[0].FollowTasks {
		t.Fatalf("follows = %+v", *follows)
	}

	// Unbinding puts the general group back.
	if _, err := s.Command([]string{"remove", "--path", dest}); err != nil {
		t.Fatal(err)
	}
	if pol, ok, _ := s.Store.GetGroupPolicy("telegram", "-1001"); !ok || pol.Kind != store.GroupGeneral {
		t.Fatalf("policy after remove = %+v %v", pol, ok)
	}
}

func TestDirAddFolderInRepoIsProjectAndRollsBack(t *testing.T) {
	s, _, _, _ := trackService(t, acmeGroup)
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.failAt = func(name string) error {
		if name == "listen" {
			return errors.New("boom")
		}
		return nil
	}
	if _, err := s.Dir("telegram", "Acme", dest, false); err == nil {
		t.Fatal("no error")
	}
	if exists(HistoryPath(dest)) || exists(filepath.Join(dest, DotfileName)) || exists(filepath.Join(dest, ".gitignore")) {
		t.Fatal("files left after rollback")
	}
	if _, ok, _ := s.Store.ProjectForGroup("telegram", "-1001"); ok {
		t.Fatal("binding left after rollback")
	}
	if _, ok, _ := s.Store.GetGroupPolicy("telegram", "-1001"); ok {
		t.Fatal("an off group got a policy")
	}
	if _, ok, _ := s.Store.GroupFolder("telegram", "-1001"); ok {
		t.Fatal("group folder left after rollback")
	}

	s.failAt = nil
	msg, err := s.Dir("telegram", "Acme", dest, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "(project)") {
		t.Fatalf("reply = %q", msg)
	}
}

func TestDirValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	other := client.Group{Name: "Other", Transport: "telegram", NativeID: "-1002"}
	s, _, p, _ := trackedAcme(t, other)

	file := filepath.Join(home, "file")
	_ = os.WriteFile(file, nil, 0o600)
	withDot := filepath.Join(home, "withdot")
	_ = os.MkdirAll(withDot, 0o700)
	_ = os.WriteFile(filepath.Join(withDot, DotfileName), nil, 0o600)
	cfg := filepath.Join(home, ".config", "zcoms", "x")
	_ = os.MkdirAll(cfg, 0o700)

	cases := map[string]struct {
		path   string
		create bool
		want   string
	}{
		"relative":       {"some/dir", false, "absolute"},
		"empty":          {"  ", false, "required"},
		"missing":        {filepath.Join(home, "nope"), false, "does not exist"},
		"file":           {file, false, "not a folder"},
		"has dotfile":    {withDot, false, "already has"},
		"config":         {cfg, false, "config folder"},
		"config tilde":   {"~/.config/zcoms", false, "config folder"},
		"other's folder": {p.Path, false, "already the folder"},
		"inside other":   {filepath.Join(p.Path, "sub"), true, "inside"},
		"contains other": {filepath.Dir(p.Path), false, "contains"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.Dir("telegram", "Other", c.path, c.create)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	if _, err := s.Dir("telegram", "Acme", p.Path, false); err == nil || !strings.Contains(err.Error(), "already the folder of Acme") {
		t.Fatalf("same folder: %v", err)
	}
	if exists(filepath.Join(p.Path, "sub")) {
		t.Fatal("a refused --create made the folder")
	}

	// ~ expands, and --create makes the folder.
	if _, err := s.Dir("telegram", "Other", "~/groups/other", true); err != nil {
		t.Fatal(err)
	}
	if q, _, _ := s.Store.ProjectForGroup("telegram", "-1002"); q.Path != filepath.Join(home, "groups", "other") {
		t.Fatalf("path = %s", q.Path)
	}
}

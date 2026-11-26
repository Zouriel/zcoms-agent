package projects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

type setCall struct {
	native         string
	listen, follow *bool
}

type fakeOwner struct {
	groups map[string]client.Group // by name and by native id
	sets   []setCall
}

func (f *fakeOwner) GroupGet(_, group string) (*client.Group, error) {
	if g, ok := f.groups[group]; ok {
		return &g, nil
	}
	return nil, nil
}

func (f *fakeOwner) GroupMembers(string, string) ([]client.GroupMember, error) {
	return []client.GroupMember{{ID: "100", Name: "Ibrahim"}, {ID: "1", Name: "Me", IsSelf: true}}, nil
}

func (f *fakeOwner) GroupSet(_, group string, listen, follow *bool) (*client.Group, error) {
	f.sets = append(f.sets, setCall{group, listen, follow})
	g := f.groups[group]
	return &g, nil
}

func (f *fakeOwner) SendOpts(string, string, string, string, []string) (client.Response, error) {
	return client.Response{OK: true}, nil
}

func (f *fakeOwner) ListContacts() ([]client.Contact, error) { return nil, nil }

func (f *fakeOwner) last() setCall { return f.sets[len(f.sets)-1] }

func trackService(t *testing.T, groups ...client.Group) (*Service, *fakeOwner, string, *[]store.Project) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	base := filepath.Join(t.TempDir(), "zcoms-groups")
	if err := st.SetSetting(store.Owner, GroupsDirKey, base); err != nil {
		t.Fatal(err)
	}
	fo := &fakeOwner{groups: map[string]client.Group{}}
	for _, g := range groups {
		fo.groups[g.Name] = g
		fo.groups[g.NativeID] = g
	}
	var follows []store.Project
	s := &Service{Store: st, Owner: fo, OnFollow: func(p store.Project) { follows = append(follows, p) }}
	return s, fo, base, &follows
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Acme build":            "acme-build",
		"  Hello, World!! 2026": "hello-world-2026",
		"ދިވެހި":                "group",
		"---":                   "group",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrackOnCreatesFolderAndBinds(t *testing.T) {
	acme := client.Group{Name: "Acme build", Transport: "telegram", NativeID: "-1001"}
	acme2 := client.Group{Name: "Acme Build!", Transport: "telegram", NativeID: "-1002"}
	s, fo, base, follows := trackService(t, acme, acme2)

	if _, err := s.Track("telegram", "Acme build", true, false); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(base, "acme-build")
	for _, f := range []string{DotfileName, filepath.Join(HistoryDir, "history.db")} {
		if _, err := os.Stat(filepath.Join(folder, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	dot, _, _ := ReadDotfile(folder)
	if dot.Group != "Acme build" || !dot.FollowTasks || dot.member("Ibrahim") == nil || dot.member("Me") != nil {
		t.Fatalf(".zcoms = %+v", dot)
	}
	p, ok, _ := s.Store.ProjectForGroup("telegram", "-1001")
	if !ok || !p.Tracked || !p.FollowTasks || p.Path != folder || p.RestoreGeneral {
		t.Fatalf("project = %+v %v", p, ok)
	}
	if pol, _, _ := s.Store.GetGroupPolicy("telegram", "-1001"); pol.Kind != store.GroupProject || pol.ProjectID != p.ID {
		t.Fatalf("policy = %+v", pol)
	}
	if c := fo.last(); c.native != "-1001" || !*c.listen || !*c.follow {
		t.Fatalf("group_set = %+v", c)
	}
	if len(*follows) != 1 || !(*follows)[0].FollowTasks {
		t.Fatalf("standup not scheduled: %+v", *follows)
	}
	if files, _ := filepath.Glob(filepath.Join(folder, "*.md")); len(files) != 0 {
		t.Fatalf("instruction files written in a folder with no repo: %v", files)
	}

	// A second group with the same slug gets its own folder.
	if _, err := s.Track("telegram", "-1002", true, false); err != nil {
		t.Fatal(err)
	}
	p2, _, _ := s.Store.ProjectForGroup("telegram", "-1002")
	if p2.Path != filepath.Join(base, "acme-build-2") {
		t.Fatalf("second folder = %s", p2.Path)
	}
	// Tracking twice is a no-op.
	if _, err := s.Track("telegram", "Acme build", true, false); err != nil {
		t.Fatal(err)
	}
}

func TestTrackedKindFollowsRepo(t *testing.T) {
	acme := client.Group{Name: "Acme", Transport: "telegram", NativeID: "-1001"}
	s, _, _, _ := trackService(t, acme)
	if _, err := s.Track("telegram", "Acme", true, false); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.Store.ProjectForGroup("telegram", "-1001")
	if KindOf(p) != KindTasks {
		t.Fatalf("kind = %s, want tasks", KindOf(p))
	}
	h, _ := OpenHistory(p.Path)
	_, _ = h.AddTask("Book the venue", 0, "")
	h.Close()
	pc, err := s.CallContext(p.ID)
	if err != nil || !pc.General || pc.Dir != "" || len(pc.Profile.Tools) != 0 {
		t.Fatalf("no-repo call context = %+v %v", pc, err)
	}
	if !contains(pc.Notes, "Book the venue") || contains(pc.Notes, "sqlite") {
		t.Fatalf("notes = %q", pc.Notes)
	}

	// The owner drops a repo into the folder: next call is a project call.
	repo := filepath.Join(p.Path, "app")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if KindOf(p) != KindProject {
		t.Fatalf("kind = %s, want project", KindOf(p))
	}
	pc, err = s.CallContext(p.ID)
	if err != nil || pc.General || pc.Dir != p.Path || !pc.Profile.HasShell() {
		t.Fatalf("repo call context = %+v %v", pc, err)
	}
	if !pc.CanWrite {
		t.Fatal("an uncapped workspace refused edits")
	}
	ws, ok, _ := s.Store.GetWorkspaceByPath(repo)
	if !ok {
		t.Fatal("repo not registered as a workspace")
	}
	for role, want := range map[string]bool{"confirm": false, "edit": true, "full": true} {
		r := role
		if err := s.Store.UpdateWorkspace(store.Owner, ws.ID, nil, &r, nil, nil); err != nil {
			t.Fatal(err)
		}
		if pc, _ = s.CallContext(p.ID); pc.CanWrite != want {
			t.Fatalf("max_role %s: CanWrite = %v", role, pc.CanWrite)
		}
	}
	// The repo's max_role caps the folder.
	role := "read"
	if err := s.Store.UpdateWorkspace(store.Owner, ws.ID, nil, &role, nil, nil); err != nil {
		t.Fatal(err)
	}
	pc, _ = s.CallContext(p.ID)
	if pc.Profile.HasShell() || pc.MaxRole != "read" || pc.CanWrite {
		t.Fatalf("max_role read still gave a shell: %+v", pc)
	}
}

func TestTrackOffKeepsDataAndRestoresGeneral(t *testing.T) {
	acme := client.Group{Name: "Acme", Transport: "telegram", NativeID: "-1001"}
	s, fo, _, _ := trackService(t, acme)
	if err := s.Store.SetGroupPolicy(store.Owner, store.GroupPolicy{Transport: "telegram", NativeID: "-1001", Name: "Acme", Kind: store.GroupGeneral}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Track("telegram", "Acme", true, false); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.Store.ProjectForGroup("telegram", "-1001")
	if !p.RestoreGeneral {
		t.Fatal("general state before tracking not remembered")
	}
	if _, err := s.Track("telegram", "Acme", false, false); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Store.ProjectForGroup("telegram", "-1001"); ok {
		t.Fatal("still bound")
	}
	if pol, ok, _ := s.Store.GetGroupPolicy("telegram", "-1001"); !ok || pol.Kind != store.GroupGeneral {
		t.Fatalf("policy after off = %+v %v", pol, ok)
	}
	if c := fo.last(); !*c.listen || *c.follow {
		t.Fatalf("group_set after off = listen %v follow %v", *c.listen, *c.follow)
	}
	if _, err := os.Stat(HistoryPath(p.Path)); err != nil {
		t.Fatalf("history deleted without purge: %v", err)
	}

	// On again reuses the folder; off with purge deletes the data and the
	// now-empty folder, and a group that was off before goes back to off.
	if err := s.Store.DeleteGroupPolicy(store.Owner, "telegram", "-1001"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Track("telegram", "Acme", true, false); err != nil {
		t.Fatal(err)
	}
	if q, _, _ := s.Store.ProjectForGroup("telegram", "-1001"); q.Path != p.Path {
		t.Fatalf("folder not reused: %s vs %s", q.Path, p.Path)
	}
	if _, err := s.Track("telegram", "Acme", false, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Store.GetGroupPolicy("telegram", "-1001"); ok {
		t.Fatal("group still on after tracking off from an off group")
	}
	if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
		t.Fatalf("purged folder still there: %v", err)
	}
}

func TestPurgeKeepsOwnerFiles(t *testing.T) {
	acme := client.Group{Name: "Acme", Transport: "telegram", NativeID: "-1001"}
	s, _, _, _ := trackService(t, acme)
	if _, err := s.Track("telegram", "Acme", true, false); err != nil {
		t.Fatal(err)
	}
	p, _, _ := s.Store.ProjectForGroup("telegram", "-1001")
	keep := filepath.Join(p.Path, "notes.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Track("telegram", "Acme", false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("purge removed the owner's file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.Path, HistoryDir)); !os.IsNotExist(err) {
		t.Fatal("history kept after purge")
	}
}

func TestTrackRefusesProjectInitGroups(t *testing.T) {
	acme := client.Group{Name: "Acme", Transport: "telegram", NativeID: "-1001"}
	s, _, _, _ := trackService(t, acme)
	dir := t.TempDir()
	_ = s.Store.UpsertDiscovered(dir, "acme")
	ws, _, _ := s.Store.GetWorkspaceByPath(dir)
	if _, err := s.Store.CreateProject(store.Owner, store.Project{WorkspaceID: ws.ID, Transport: "telegram", NativeID: "-1001", GroupName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Track("telegram", "Acme", true, false); err == nil {
		t.Fatal("tracking took over a project group")
	}
	if _, err := s.Track("telegram", "Acme", false, false); err == nil {
		t.Fatal("track off unbound a project init group")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

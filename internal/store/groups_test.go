package store

import "testing"

func TestGroupPolicyOwnerOnly(t *testing.T) {
	s := openTest(t)
	p := GroupPolicy{Transport: "telegram", NativeID: "-100", Name: "Fam", Kind: GroupGeneral}
	if err := s.SetGroupPolicy(Agent, p); err == nil {
		t.Fatal("agent wrote a group policy")
	}
	if err := s.SetGroupPolicy(Owner, p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetGroupPolicy("telegram", "-100")
	if err != nil || !ok || got.Kind != GroupGeneral {
		t.Fatalf("get = %+v %v %v", got, ok, err)
	}
	if err := s.DeleteGroupPolicy(Agent, "telegram", "-100"); err == nil {
		t.Fatal("agent deleted a group policy")
	}
	if err := s.DeleteGroupPolicy(Owner, "telegram", "-100"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetGroupPolicy("telegram", "-100"); ok {
		t.Fatal("policy survived delete")
	}
}

func TestProjectBindingByPath(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertDiscovered("/src/acme", "acme"); err != nil {
		t.Fatal(err)
	}
	ws, _, _ := s.GetWorkspaceByPath("/src/acme")
	p, err := s.CreateProject(Owner, Project{WorkspaceID: ws.ID, Transport: "telegram", NativeID: "-5", GroupName: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(Owner, Project{WorkspaceID: ws.ID, Transport: "telegram", NativeID: "-6"}); err == nil {
		t.Fatal("folder bound twice")
	}
	pol, ok, _ := s.GetGroupPolicy("telegram", "-5")
	if !ok || pol.Kind != GroupProject || pol.ProjectID != p.ID {
		t.Fatalf("project policy = %+v %v", pol, ok)
	}
	for path, want := range map[string]bool{"/src/acme": true, "/src/acme/web/x": true, "/src/acmeother": false, "/src": false} {
		if _, ok, _ := s.ProjectForPath(path); ok != want {
			t.Errorf("ProjectForPath(%q) = %v, want %v", path, ok, want)
		}
	}
	if err := s.DeleteProject(Owner, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.GetGroupPolicy("telegram", "-5"); ok {
		t.Fatal("policy survived project removal")
	}
}

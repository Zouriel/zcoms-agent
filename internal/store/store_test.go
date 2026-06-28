package store

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The crown-jewel tables reject the agent caller and accept the owner.
func TestPersonaGuard(t *testing.T) {
	s := openTest(t)
	p := Persona{Key: "bridge", Backend: "claude", SeedPrompt: "be helpful"}

	if _, err := s.CreatePersona(Agent, p); err == nil {
		t.Fatal("agent must NOT create a persona")
	} else if _, ok := err.(ErrForbidden); !ok {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if _, err := s.CreatePersona(Owner, p); err != nil {
		t.Fatalf("owner must create a persona: %v", err)
	}
	// agent cannot edit or delete either
	if err := s.UpdatePersona(Agent, "bridge", p); err == nil {
		t.Fatal("agent must NOT update a persona")
	}
	if err := s.DeletePersona(Agent, "bridge"); err == nil {
		t.Fatal("agent must NOT delete a persona")
	}
}

func TestAllowlistGuard(t *testing.T) {
	s := openTest(t)
	e := AllowEntry{Platform: "telegram", Handle: "@ali", MaxRole: "edit"}
	if _, err := s.CreateAllow(Agent, e); err == nil {
		t.Fatal("agent must NOT add to the allowlist")
	}
	got, err := s.CreateAllow(Owner, e)
	if err != nil {
		t.Fatalf("owner allow: %v", err)
	}
	if _, ok, _ := s.Allowed("telegram", "@ali"); !ok {
		t.Fatal("entry should be allow-listed")
	}
	if err := s.DeleteAllow(Agent, got.ID); err == nil {
		t.Fatal("agent must NOT remove an allowlist entry")
	}
}

func TestSettingsGuard(t *testing.T) {
	s := openTest(t)
	if err := s.SetSetting(Agent, "discovery_roots", "/etc"); err == nil {
		t.Fatal("agent must NOT write settings")
	}
	if err := s.SetSetting(Owner, "discovery_roots", "/home/me/src"); err != nil {
		t.Fatalf("owner settings: %v", err)
	}
	if v, _ := s.GetSetting("discovery_roots"); v != "/home/me/src" {
		t.Fatalf("got %q", v)
	}
}

// Workspaces: name/pinned/ignored are agent-writable; max_role is owner-only.
func TestWorkspaceGuard(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertDiscovered("/home/me/repo", "repo"); err != nil {
		t.Fatalf("discover: %v", err)
	}
	w, ok, _ := s.GetWorkspaceByPath("/home/me/repo")
	if !ok {
		t.Fatal("workspace not discovered")
	}
	// agent may rename + ignore
	name := "myrepo"
	ignored := true
	if err := s.UpdateWorkspace(Agent, w.ID, &name, nil, nil, &ignored); err != nil {
		t.Fatalf("agent rename/ignore should be allowed: %v", err)
	}
	// agent may NOT change the permission cap
	cap := "full"
	if err := s.UpdateWorkspace(Agent, w.ID, nil, &cap, nil, nil); err == nil {
		t.Fatal("agent must NOT change workspace max_role")
	}
	if err := s.UpdateWorkspace(Owner, w.ID, nil, &cap, nil, nil); err != nil {
		t.Fatalf("owner cap change: %v", err)
	}
	// invalid enum rejected
	bad := "superuser"
	if err := s.UpdateWorkspace(Owner, w.ID, nil, &bad, nil, nil); err == nil {
		t.Fatal("invalid max_role must be rejected")
	}
}

// Sessions have no create/delete; label is agent-writable decoration only.
func TestSessionDecorationOnly(t *testing.T) {
	s := openTest(t)
	_ = s.UpsertDiscovered("/w", "w")
	w, _, _ := s.GetWorkspaceByPath("/w")
	if err := s.SetLabel(Agent, w.ID, "claude", "sess-123", "refactor"); err != nil {
		t.Fatalf("agent label: %v", err)
	}
	decs, _ := s.ListSessionDecorations(w.ID)
	if decs["sess-123"].Label != "refactor" {
		t.Fatalf("label not stored: %+v", decs)
	}
	if err := s.SetLabel(Agent, w.ID, "bogus", "x", "y"); err == nil {
		t.Fatal("invalid backend must be rejected")
	}
}

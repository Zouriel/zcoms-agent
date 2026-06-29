package store

import "testing"

func TestTriageGroupCRUD(t *testing.T) {
	s := openTest(t)

	// Agent (untrusted) may not write — crown jewel.
	if _, err := s.SaveTriageGroup(Agent, TriageGroup{Name: "x", ScheduleSpec: "1h"}); err == nil {
		t.Fatal("agent must NOT save a triage group")
	}

	g, err := s.SaveTriageGroup(Owner, TriageGroup{
		Name: "Work", ScheduleKind: "daily", ScheduleSpec: "09:00,18:00", Enabled: true,
		Sources: []TriageSource{{Transport: "telegram"}, {Transport: "whatsapp", ChatFilter: "team"}},
	})
	if err != nil {
		t.Fatalf("owner save: %v", err)
	}
	if g.ID == 0 {
		t.Fatal("new group should get an id")
	}

	groups, err := s.ListTriageGroups()
	if err != nil || len(groups) != 1 {
		t.Fatalf("list: %v (%d groups)", err, len(groups))
	}
	got := groups[0]
	if got.Name != "Work" || got.ScheduleKind != "daily" || !got.Enabled || len(got.Sources) != 2 {
		t.Fatalf("group round-trip wrong: %+v", got)
	}

	// Update replaces sources atomically.
	got.Sources = []TriageSource{{Transport: "telegram"}}
	got.Enabled = false
	if _, err := s.SaveTriageGroup(Owner, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	groups, _ = s.ListTriageGroups()
	if len(groups[0].Sources) != 1 || groups[0].Enabled {
		t.Fatalf("update didn't replace sources/flag: %+v", groups[0])
	}

	// Enable toggle + delete.
	if err := s.SetTriageGroupEnabled(Owner, g.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTriageGroup(Owner, g.ID); err != nil {
		t.Fatal(err)
	}
	groups, _ = s.ListTriageGroups()
	if len(groups) != 0 {
		t.Fatalf("delete left %d groups", len(groups))
	}

	// Validation: bad schedule kind rejected.
	if _, err := s.SaveTriageGroup(Owner, TriageGroup{Name: "z", ScheduleKind: "weekly", ScheduleSpec: "x"}); err == nil {
		t.Fatal("invalid schedule kind accepted")
	}
}

func TestEnsureDefaultTriageGroup(t *testing.T) {
	s := openTest(t)

	if err := s.EnsureDefaultTriageGroup("2h", true, true); err != nil {
		t.Fatalf("ensure default: %v", err)
	}
	groups, _ := s.ListTriageGroups()
	if len(groups) != 1 {
		t.Fatalf("want 1 default group, got %d", len(groups))
	}
	d := groups[0]
	if d.Name != "Default" || d.ScheduleSpec != "2h" || !d.Enabled {
		t.Fatalf("default group wrong: %+v", d)
	}
	if len(d.Sources) != 2 { // telegram + whatsapp (enabled)
		t.Fatalf("want telegram+whatsapp sources, got %d", len(d.Sources))
	}

	// Idempotent: a second call must not add another group.
	if err := s.EnsureDefaultTriageGroup("1h", true, false); err != nil {
		t.Fatal(err)
	}
	groups, _ = s.ListTriageGroups()
	if len(groups) != 1 {
		t.Fatalf("ensure default not idempotent: %d groups", len(groups))
	}
}

package agentd

import (
	"log"
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

func TestGroupJoinedTurnsOnGeneral(t *testing.T) {
	a := testAgent(t)
	a.log = log.New(&strings.Builder{}, "", 0)
	for _, tp := range []string{"telegram", "whatsapp"} {
		id := "-1009"
		if tp == "whatsapp" {
			id = "120363000000000009@g.us"
		}
		a.handleGroupJoined(client.Event{Event: client.EventGroupJoined, Transport: tp, Address: id, GroupName: "Crew", Group: true})
		pol, ok, err := a.Store.GetGroupPolicy(tp, id)
		if err != nil || !ok || pol.Kind != store.GroupGeneral || pol.Name != "Crew" {
			t.Fatalf("%s: policy = %+v %v %v", tp, pol, ok, err)
		}
	}

	// An existing policy is never replaced by a join notice.
	_ = a.Store.UpsertDiscovered(t.TempDir(), "x")
	ws, _ := a.Store.ListWorkspaces(true)
	if _, err := a.Store.CreateProject(store.Owner, store.Project{WorkspaceID: ws[0].ID, Transport: "telegram", NativeID: "-2002", GroupName: "Proj"}); err != nil {
		t.Fatal(err)
	}
	a.handleGroupJoined(client.Event{Event: client.EventGroupJoined, Transport: "telegram", Address: "-2002", GroupName: "Proj"})
	if pol, _, _ := a.Store.GetGroupPolicy("telegram", "-2002"); pol.Kind != store.GroupProject {
		t.Fatalf("join notice changed a project group: %+v", pol)
	}

	// Unknown transports are ignored.
	a.handleGroupJoined(client.Event{Event: client.EventGroupJoined, Transport: "instagram", Address: "x"})
	if _, ok, _ := a.Store.GetGroupPolicy("instagram", "x"); ok {
		t.Fatal("policy for an unknown transport")
	}
}

func TestTrackCmdUsage(t *testing.T) {
	a := testAgent(t)
	for _, line := range []string{
		"group track", "group track maybe telegram Acme", "group track on signal Acme",
		"group track on telegram", "group track on telegram Acme --purge",
	} {
		if _, err := a.groupCmd(line); err == nil {
			t.Errorf("%q: no error", line)
		}
	}
}

func TestRoleCmdUsage(t *testing.T) {
	a := testAgent(t)
	for _, line := range []string{
		"group role", "group role telegram Acme 77", "group role telegram Acme 77 admin",
		"group role signal Acme 77 write",
	} {
		if _, err := a.groupCmd(line); err == nil {
			t.Errorf("%q: no error", line)
		}
	}
	// The store refuses a role set by anyone but the owner.
	if err := a.Store.SetGroupRole(store.Agent, "telegram", "-100", "77", store.RoleWrite); err == nil {
		t.Fatal("agent caller set a role")
	}
}

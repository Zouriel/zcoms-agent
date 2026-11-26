package agentd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/cmdline"
	"github.com/Zouriel/zcoms-agent/internal/groups"
	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/projects"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// handleGroupEvent hands a group call to the groups handler. Group events
// never reach a 1:1 subscriber.
func (a *Agent) handleGroupEvent(ev client.Event) {
	if a.Groups != nil {
		a.Groups.Handle(ev)
	}
}

// errNoBackend is returned when no agent CLI can run a restricted turn.
var errNoBackend = errors.New("no agent CLI can run this turn")

// runRestricted runs one tool-restricted, never-resumed turn on a persona's
// brain. A backend that cannot honour the tool profile exactly (codex always
// has a shell, agy always has web search and can only be pinned to web
// access, never to single attached files) falls back to claude; with neither, the caller gets an error
// and sends its canned reply.
func (a *Agent) runRestricted(req groups.RunRequest) (string, error) {
	brain := personas.Brain(a.Store, req.Persona)
	if brain.Backend == "" {
		return "", errNoBackend
	}
	if !runner.Supports(brain.Backend, req.Profile) {
		if !runner.AgentAvailable(runner.BackendClaude) {
			return "", fmt.Errorf("%w: %s cannot run without a shell and claude is not installed", errNoBackend, brain.Backend)
		}
		brain = runner.Brain{Backend: runner.BackendClaude, Model: personas.DefaultModel(runner.BackendClaude, req.Persona)}
	}
	res, err := runner.RunRestricted(brain, req.Dir, req.Prompt, req.Profile)
	return res.Text, err
}

// groupScratch is the parent of the empty per-call dirs general group calls
// run in.
func groupScratch() string {
	d := filepath.Join(os.TempDir(), "zcoms-group-calls")
	_ = os.MkdirAll(d, 0o700)
	return d
}

// groupCmd handles `group on|off <transport> <group>` and `group list`. Owner
// only: agent.sock is owner-only and refuses group-call processes.
func (a *Agent) groupCmd(text string) (string, error) {
	words, err := cmdline.Split(text)
	if err != nil {
		return "", err
	}
	words = words[1:]
	usage := errors.New("usage: group on|off <telegram|whatsapp> <group> | group track on|off <telegram|whatsapp> <group> [--purge] | group role <telegram|whatsapp> <group> <member id> read|write | group dir <telegram|whatsapp> <group> <absolute folder> [--create] | group list")
	if len(words) == 0 {
		return "", usage
	}
	switch words[0] {
	case "list", "ls":
		ps, err := a.groupPolicyViews()
		if err != nil {
			return "", err
		}
		if len(ps) == 0 {
			return "No groups are on.", nil
		}
		var b strings.Builder
		for _, p := range ps {
			fmt.Fprintf(&b, "%s  %s  %s", p.Kind, p.Transport, p.Name)
			if p.Folder != "" {
				fmt.Fprintf(&b, "  %s", p.Folder)
			}
			b.WriteString("\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "track":
		return a.trackCmd(words[1:])
	case "role":
		return a.roleCmd(words[1:])
	case "dir", "folder":
		return a.dirCmd(words[1:])
	case "on", "off":
	default:
		return "", usage
	}
	if len(words) < 3 {
		return "", usage
	}
	tp := strings.ToLower(words[1])
	if !runner.IsAllowPlatform(tp) {
		return "", fmt.Errorf("unknown transport %q (want telegram or whatsapp)", words[1])
	}
	name := strings.Join(words[2:], " ")
	g, err := a.Owner.GroupGet(tp, name)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", fmt.Errorf("no %s group called %q", tp, name)
	}
	pol, has, err := a.Store.GetGroupPolicy(tp, g.NativeID)
	if err != nil {
		return "", err
	}
	if has && pol.Kind == store.GroupProject {
		if p, ok, _ := a.Store.ProjectForGroup(tp, g.NativeID); ok && p.Tracked {
			return "", fmt.Errorf("%s has task tracking on; turn tracking off first (group track off)", g.Name)
		}
		return "", fmt.Errorf("%s is a project group; use zc project remove inside the project folder", g.Name)
	}
	on := words[0] == "on"
	if on {
		if err := a.Store.SetGroupPolicy(store.Owner, store.GroupPolicy{Transport: tp, NativeID: g.NativeID, Name: g.Name, Kind: store.GroupGeneral}); err != nil {
			return "", err
		}
	} else if err := a.Store.DeleteGroupPolicy(store.Owner, tp, g.NativeID); err != nil {
		return "", err
	}
	listen := on
	if _, err := a.Owner.GroupSet(tp, g.NativeID, &listen, nil); err != nil {
		return "", fmt.Errorf("saved, but could not set listen on the comms side: %w", err)
	}
	if on {
		return fmt.Sprintf("On: I will answer when someone in %s mentions me or replies to me.", g.Name), nil
	}
	return fmt.Sprintf("Off: I will ignore %s.", g.Name), nil
}

// trackCmd handles `group track on|off <transport> <group> [--purge]`. Owner
// only, like every agent.sock line; the console is the one place that sends it.
func (a *Agent) trackCmd(words []string) (string, error) {
	usage := errors.New("usage: group track on|off <telegram|whatsapp> <group> [--purge]")
	args, err := cmdline.Parse(words, "purge")
	if err != nil {
		return "", err
	}
	pos := args.Pos
	if len(pos) < 3 || (pos[0] != "on" && pos[0] != "off") {
		return "", usage
	}
	tp := strings.ToLower(pos[1])
	if !runner.IsAllowPlatform(tp) {
		return "", fmt.Errorf("unknown transport %q (want telegram or whatsapp)", pos[1])
	}
	on := pos[0] == "on"
	if on && args.Has("purge") {
		return "", errors.New("--purge only goes with track off")
	}
	return a.Projects.Track(tp, strings.Join(pos[2:], " "), on, args.Has("purge"))
}

// dirCmd handles `group dir <transport> <group> <absolute folder> [--create]`:
// it gives a group without a folder one, or moves a bound group's folder.
// Owner only, like every agent.sock line; the path checks live in projects.
func (a *Agent) dirCmd(words []string) (string, error) {
	usage := errors.New("usage: group dir <telegram|whatsapp> <group> <absolute folder> [--create]")
	args, err := cmdline.Parse(words, "create")
	if err != nil {
		return "", err
	}
	pos := args.Pos
	if len(pos) < 3 {
		return "", usage
	}
	tp := strings.ToLower(pos[0])
	if !runner.IsAllowPlatform(tp) {
		return "", fmt.Errorf("unknown transport %q (want telegram or whatsapp)", pos[0])
	}
	if a.Owner == nil {
		return "", errors.New("the comms daemon is not connected")
	}
	return a.Projects.Dir(tp, strings.Join(pos[1:len(pos)-1], " "), pos[len(pos)-1], args.Has("create"))
}

// roleCmd handles `group role <transport> <group> <member id> read|write`.
// Owner only (agent.sock); the store refuses any other caller as well. The
// owner's own role is fixed at write and cannot be set.
func (a *Agent) roleCmd(words []string) (string, error) {
	usage := errors.New("usage: group role <telegram|whatsapp> <group> <member id> read|write")
	if len(words) < 4 {
		return "", usage
	}
	role := strings.ToLower(words[len(words)-1])
	member := strings.TrimSpace(words[len(words)-2])
	if role != store.RoleRead && role != store.RoleWrite {
		return "", usage
	}
	tp := strings.ToLower(words[0])
	if !runner.IsAllowPlatform(tp) {
		return "", fmt.Errorf("unknown transport %q (want telegram or whatsapp)", words[0])
	}
	if a.Owner == nil {
		return "", errors.New("the comms daemon is not connected")
	}
	g, err := a.Owner.GroupGet(tp, strings.Join(words[1:len(words)-2], " "))
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", fmt.Errorf("no %s group called %q", tp, strings.Join(words[1:len(words)-2], " "))
	}
	ms, err := a.Owner.GroupMembers(tp, g.NativeID)
	if err != nil {
		return "", fmt.Errorf("reading the group's members: %w", err)
	}
	var name string
	found := false
	for _, m := range ms {
		if m.ID != member {
			continue
		}
		if m.IsOwner {
			return "", errors.New("the owner is always write in every group")
		}
		if m.IsSelf {
			return "", errors.New("that is the agent's own account")
		}
		name, found = m.Name, true
	}
	if !found {
		return "", fmt.Errorf("%s is not a member of %s", member, g.Name)
	}
	if err := a.Store.SetGroupRole(store.Owner, tp, g.NativeID, member, role); err != nil {
		return "", err
	}
	if name == "" {
		name = member
	}
	return fmt.Sprintf("%s is now %s in %s.", name, role, g.Name), nil
}

// groupPolicyView is a group policy as the console and `group list` show it:
// kind is general, tasks (a tracked folder with no repo) or project, and
// Folder is the bound folder of a tasks or project group.
type groupPolicyView struct {
	store.GroupPolicy
	Folder  string `json:"folder,omitempty"`
	Tracked bool   `json:"tracked,omitempty"`
}

func (a *Agent) groupPolicyViews() ([]groupPolicyView, error) {
	ps, err := a.Store.ListGroupPolicies()
	if err != nil {
		return nil, err
	}
	projs, err := a.Store.ListProjects()
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]store.Project, len(projs))
	for _, p := range projs {
		byID[p.ID] = p
	}
	out := make([]groupPolicyView, 0, len(ps))
	for _, pol := range ps {
		v := groupPolicyView{GroupPolicy: pol}
		if pol.Kind == store.GroupProject {
			if p, ok := byID[pol.ProjectID]; ok {
				v.Folder, v.Tracked = p.Path, p.Tracked
				v.Kind = projects.KindOf(p)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// handleGroupJoined turns on, as a general group, a group the account was
// just added to, joined or created (owner standing instruction: every group
// the account is in answers calls with web-only reach). A group that already
// has a policy keeps it. Nothing in the event can choose the kind.
func (a *Agent) handleGroupJoined(ev client.Event) {
	tp := ev.Transport
	if tp == "" {
		tp = "telegram"
	}
	if !runner.IsAllowPlatform(tp) || ev.Address == "" {
		return
	}
	if _, has, err := a.Store.GetGroupPolicy(tp, ev.Address); err != nil {
		a.log.Printf("group joined %s/%s: %v", tp, ev.Address, err)
		return
	} else if has {
		a.log.Printf("group joined %s %q: already has a policy, left as is", tp, ev.GroupName)
		return
	}
	if err := a.Store.SetGroupPolicy(store.Owner, store.GroupPolicy{
		Transport: tp, NativeID: ev.Address, Name: ev.GroupName, Kind: store.GroupGeneral,
	}); err != nil {
		a.log.Printf("group joined %s/%s: %v", tp, ev.Address, err)
		return
	}
	listen := true
	if a.Owner != nil {
		if _, err := a.Owner.GroupSet(tp, ev.Address, &listen, nil); err != nil {
			a.log.Printf("group joined %s %q: policy saved but comms listen failed: %v", tp, ev.GroupName, err)
			return
		}
	}
	a.log.Printf("group joined: %s %q (%s) is now a general group", tp, ev.GroupName, ev.Address)
}

// projectCmd runs a `project …` line from the CLI.
func (a *Agent) projectCmd(text string) (string, error) {
	words, err := cmdline.Split(text)
	if err != nil {
		return "", err
	}
	return a.Projects.Command(words[1:])
}

// scheduleStandup registers the 09:00 Indian/Maldives standup for a followed
// project once per process. The job itself re-checks the binding and follow
// state each day, so an unfollowed or removed project simply skips; the
// scheduler's per-day guard and the standups table keep it to once a day.
func (a *Agent) scheduleStandup(p store.Project) {
	if !p.FollowTasks {
		return
	}
	a.standupMu.Lock()
	defer a.standupMu.Unlock()
	if a.standupJobs == nil {
		a.standupJobs = map[int64]bool{}
	}
	if a.standupJobs[p.ID] {
		return
	}
	a.standupJobs[p.ID] = true
	id := p.ID
	a.Sched.DailyAtNoCatchUp(fmt.Sprintf("project-standup-%d", id), 9, 0, projects.StandupZone(), func() {
		go a.Projects.StandupByID(id)
	})
}

// registerStandups schedules every followed project at start.
func (a *Agent) registerStandups() {
	ps, err := a.Store.ListProjects()
	if err != nil {
		a.log.Printf("projects: %v", err)
		return
	}
	for _, p := range ps {
		a.scheduleStandup(p)
	}
}

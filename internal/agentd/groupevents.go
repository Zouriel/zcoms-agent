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
	usage := errors.New("usage: group on|off <telegram|whatsapp> <group> | group list")
	if len(words) == 0 {
		return "", usage
	}
	switch words[0] {
	case "list", "ls":
		ps, err := a.Store.ListGroupPolicies()
		if err != nil {
			return "", err
		}
		if len(ps) == 0 {
			return "No groups are on.", nil
		}
		var b strings.Builder
		for _, p := range ps {
			fmt.Fprintf(&b, "%s  %s  %s\n", p.Kind, p.Transport, p.Name)
		}
		return strings.TrimRight(b.String(), "\n"), nil
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
	a.Sched.DailyAt(fmt.Sprintf("project-standup-%d", id), 9, 0, projects.StandupZone(), func() {
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

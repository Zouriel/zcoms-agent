// Package agentd is the unified agent process: it folds the old bridge, triage,
// and errands components into one binary on a single comms harness connection,
// owns agent.db, runs the scheduler, and serves agent.sock for the CLI/modules.
package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/bootstrap"
	"github.com/Zouriel/zcoms-agent/internal/bridge"
	"github.com/Zouriel/zcoms-agent/internal/errands"
	"github.com/Zouriel/zcoms-agent/internal/groups"
	"github.com/Zouriel/zcoms-agent/internal/lineup"
	"github.com/Zouriel/zcoms-agent/internal/morning"
	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/projects"
	"github.com/Zouriel/zcoms-agent/internal/reminders"
	"github.com/Zouriel/zcoms-agent/internal/reschedule"
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/sessions"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms-agent/internal/triage"
	"github.com/Zouriel/zcoms-agent/internal/workspaces"
	"github.com/Zouriel/zcoms-agent/scheduler"
	"github.com/Zouriel/zcoms/client"
)

// Agent is the unified runtime.
type Agent struct {
	Store      *store.Store
	Registry   *workspaces.Registry
	Sessions   *sessions.Manager
	Sched      *scheduler.Scheduler
	Client     *client.Client
	Bridge     *bridge.Comp
	Errands    *errands.Comp
	Reminders  *reminders.Comp
	Morning    *morning.Comp
	Reschedule *reschedule.Comp
	Lineup     *lineup.Comp
	Groups     *groups.Handler
	Projects   *projects.Service

	standupMu   sync.Mutex
	standupJobs map[int64]bool // projects with a registered standup job

	// Owner is an owner-caller comms client. Only owner commands from
	// agent.sock use it (group on/off, project init/remove/follow), because
	// the daemon refuses group_set from the agent caller.
	Owner *client.Client

	settings runner.Settings
	log      *log.Logger
}

// Run opens agent.db, bootstraps, builds the runtimes, and serves until ctx ends.
func Run(ctx context.Context) error {
	lg := log.New(os.Stderr, "[agent] ", log.LstdFlags)
	dir, err := client.DefaultAppDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating app dir: %w", err)
	}
	st, err := store.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		return fmt.Errorf("opening agent.db: %w", err)
	}
	defer st.Close()
	if err := bootstrap.Run(st); err != nil {
		return fmt.Errorf("first-run setup: %w", err)
	}
	runMigrations(st, lg)

	c, err := client.NewDefault()
	if err != nil {
		return err
	}
	if err := c.CheckProtocol(); err != nil {
		lg.Printf("warning: %v (start/upgrade the comms daemon: zc init agent)", err)
	}

	a := &Agent{
		Store:    st,
		Registry: workspaces.New(st),
		Sessions: sessions.New(st),
		Sched:    scheduler.NewWithStore(store.SchedulerStore{S: st}),
		Client:   c.AsCaller("agent"),
		Owner:    c.AsCaller("owner"),
		log:      lg,
	}
	if err := a.buildRuntimes(); err != nil {
		return err
	}

	// One discovery-sync at start, then on the scheduler.
	if n, err := a.Registry.Sync(); err == nil {
		lg.Printf("workspace discovery: %d repo(s) present", n)
	}

	a.registerJobs()
	go a.Sched.Run(ctx)
	go a.serveCommands(ctx)

	lg.Println("agent online (bridge + triage + errands on one harness connection)")
	a.subscribe(ctx)
	return nil
}

// buildRuntimes constructs the bridge + errands runtimes from agent.db.
func (a *Agent) buildRuntimes() error {
	allow, err := a.buildAllow()
	if err != nil {
		return err
	}
	locs, err := a.buildLocations()
	if err != nil {
		return err
	}
	settings, err := a.buildSettings()
	if err != nil {
		return err
	}
	a.settings = settings

	mainChat := int64(0)
	if settings.MainUser != "" {
		if id, err := a.Client.Resolve(settings.MainUser); err == nil {
			mainChat = id
		}
	}

	if settings.MainUser != "" {
		owner := settings.MainUser
		ping := &agyPinger{now: time.Now, send: func(text string) error {
			_, err := a.Client.Send(owner, text)
			return err
		}}
		runner.AgySignedOut = ping.fire
	}

	// seedFn reads a persona's owner-editable seed prompt from agent.db live, so
	// console edits take effect without a restart.
	seedFn := func(key string) string { return personas.SeedOr(a.Store, key) }

	// Reminders is built before the bridge so the bridge can drive it in-process
	// for `remind …` commands.
	// The reminder agent resolves its backend live from the Reminders persona (so a
	// console backend change applies with no restart) and runs one turn at a time.
	remBrain := func() runner.Brain { return reminders.LiveBrain(a.Store) }
	a.Reminders = reminders.New(a.Client, a.Store, settings.MainUser, mainChat, seedFn,
		reminders.NewAgentTurn(remBrain))

	// The morning agent runs one daily briefing, resolving its backend live from
	// the Morning persona just like reminders.
	morningBrain := func() runner.Brain { return morning.LiveBrain(a.Store) }
	a.Morning = morning.New(a.Client, a.Store, settings.MainUser, mainChat, seedFn,
		morning.NewAgentTurn(morningBrain))

	// The reschedule negotiator reaches out to an event's other party on the
	// owner's behalf; backend resolves live from the Reschedule persona.
	rescheduleBrain := func() runner.Brain { return reschedule.LiveBrain(a.Store) }
	a.Reschedule = reschedule.New(a.Client, a.Store, mainChat, seedFn,
		reschedule.NewAgentTurn(rescheduleBrain))

	// The lineup is the owner's on-demand schedule assistant: opened with the
	// "lineup" keyword, it reads/adds/reschedules the owner's own events until they
	// end it or go quiet. Backend resolves live from the Lineup persona.
	lineupBrain := func() runner.Brain { return lineup.LiveBrain(a.Store) }
	a.Lineup = lineup.New(a.Client, a.Store, settings.MainUser, mainChat, seedFn,
		lineup.NewAgentTurn(lineupBrain), a.Reschedule.Start)

	// brainFn resolves a persona's backend + model from agent.db live, so a console
	// change applies to the next turn without a restart (the seed already did).
	brainFn := func(persona string, override runner.Backend) runner.Brain {
		return personas.BrainWith(a.Store, persona, override)
	}

	// The schedule agents only add to the owner's own calendar, so a "remind
	// <someone else> to …" request is handed to the reminders module instead of
	// being filed as an owner event addressed to the wrong person.
	remindOther := func(who, task string) string {
		return a.Reminders.HandleCommand(a.ownerRequester(), "remind "+who+" to "+task)
	}
	a.Morning.SetRemindOther(remindOther)
	a.Lineup.SetRemindOther(remindOther)

	a.Bridge = bridge.New(bridge.Deps{
		Client: a.Client, WAEnabled: settings.WhatsApp.Enabled,
		Locations: locs, Allow: allow, Settings: settings, MainChatID: mainChat,
		AgentBrain:  brainFn,
		PersonaSeed: seedFn, Reminders: a.Reminders,
		// Late-bound: errands is built just below, so read it at call time (a user's
		// errand command lands long after buildRuntimes finishes).
		ErrandCmd: func(text string) string {
			if a.Errands == nil {
				return "Errands aren't available right now."
			}
			return a.Errands.HandleCommand(text)
		},
		Phrase: func(key string) string { return bridge.PhraseOr(a.Store, key) },
		ProjectNote: func(path string) string {
			p, ok, err := a.Store.ProjectForPath(path)
			if err != nil || !ok {
				return ""
			}
			return projects.SessionNote(p)
		},
	})
	a.Errands = errands.New(a.Client, settings.WhatsApp.Enabled,
		func(persona string) runner.Brain { return personas.Brain(a.Store, persona) },
		mainChat, seedFn)

	// Group calls: one fresh, tool-restricted turn per call on the group
	// persona, replying only to the calling message in the calling group.
	// Projects: owner commands use the owner client; the standup reads and
	// posts as the agent. Its model passes run with no tools at all.
	a.Projects = &projects.Service{
		Store: a.Store, Owner: a.Owner, Agent: a.Client, Log: a.log,
		Run: func(persona, dir, prompt string) (string, error) {
			return a.runRestricted(groups.RunRequest{Persona: persona, Dir: dir, Prompt: personas.SeedOr(a.Store, persona) + "\n\n" + prompt})
		},
		OnFollow:  a.scheduleStandup,
		OwnerChat: func() string { return a.settings.MainUser },
	}
	a.Groups = &groups.Handler{
		Comms: a.Client, Policies: a.Store, Run: a.runRestricted, Projects: a.Projects,
		Transcribe: a.transcribeVoice,
		Seed:       seedFn, Scratch: groupScratch(), Log: a.log,
	}

	// Migrate the legacy single triage schedule into a "Default" group on first
	// run (idempotent — only seeds when no groups exist yet).
	if err := a.Store.EnsureDefaultTriageGroup(settings.Triage.Schedule, settings.Triage.Enabled, settings.WhatsApp.Enabled); err != nil {
		a.log.Printf("triage default group: %v", err)
	}
	return nil
}

// registerJobs wires the scheduler's consumers: triage interval, WhatsApp
// errand-reply poll, due scheduled errands, and periodic workspace discovery.
func (a *Agent) registerJobs() {
	// One dispatch tick reads the triage groups every minute and runs whichever
	// are due on their own schedule. Reading the DB each tick means console
	// edits (new group, schedule change, enable/disable) take effect live —
	// no agent restart needed.
	a.Sched.Interval("triage-dispatch", time.Minute, a.runDueTriageGroups)
	// WhatsApp (bridge + errands) is fully on the in-process whatsmeow transport:
	// inbound arrives over the daemon subscribe stream, so there are no sidecar
	// polls — the Node Baileys sidecar is retired.
	a.Sched.Interval("scheduled-errands", 30*time.Second, a.Errands.FireDueScheduled)
	// Reminders are advanced from one coarse tick that scans the reminders table
	// for due rows (like triage) — so a reminder that came due during downtime
	// fires on the first tick after a restart, with no in-memory state to rebuild.
	a.Sched.Interval("reminders-tick", 30*time.Second, a.Reminders.FireDue)
	// The morning briefing fires once a day around 6am local (guard persisted, so a
	// machine that was off at 6 still fires on first wake). Fire checks the live
	// enable/disable setting and spawns the session in its own goroutine, so a
	// briefing parked on the owner's reply never blocks the scheduler tick.
	a.Sched.DailyAt("morning-briefing", 6, 0, time.Local, a.Morning.Fire)
	a.registerStandups()
	a.Sched.Interval("workspace-discovery", 10*time.Minute, func() { _, _ = a.Registry.Sync() })
}

// ownerRequester builds the reminder requester for the agent.sock/CLI path: the
// caller of a 0600 socket is always the owner, so "remind me …" addresses the
// owner's own chat (resolved by the reminders runtime when the address is blank).
func (a *Agent) ownerRequester() reminders.Requester {
	return reminders.Requester{Transport: "telegram", Handle: a.settings.MainUser, Name: "you", Owner: true}
}

// reloadAllow rebuilds the allowlist from agent.db and pushes it into the live
// bridge, so an add/remove (CLI or console) takes effect immediately — no restart.
func (a *Agent) reloadAllow() {
	allow, err := a.buildAllow()
	if err != nil {
		a.log.Printf("allowlist reload: %v", err)
		return
	}
	if a.Bridge != nil {
		a.Bridge.SetAllow(allow)
	}
}

// reloadSettings rebuilds settings from agent.db and pushes them into the live
// bridge, so a settings change (e.g. toggling auto-reply or editing its text in
// the console) takes effect immediately — no restart.
func (a *Agent) reloadSettings() {
	settings, err := a.buildSettings()
	if err != nil {
		a.log.Printf("settings reload: %v", err)
		return
	}
	a.settings = settings
	if a.Bridge != nil {
		a.Bridge.SetSettings(settings)
	}
}

// transportOf returns the event's transport, defaulting to telegram for a
// pre-v2 daemon that doesn't tag events.
func transportOf(ev client.Event) string {
	if ev.Transport == "" {
		return "telegram"
	}
	return ev.Transport
}

// runDueTriageGroups runs every enabled triage group whose schedule is due,
// recording each run time. Called once a minute by the dispatch tick.
func (a *Agent) runDueTriageGroups() {
	groups, err := a.Store.ListTriageGroups()
	if err != nil {
		a.log.Printf("triage dispatch: %v", err)
		return
	}
	s, _ := a.buildSettings()
	allow, _ := a.buildAllow()
	now := time.Now()
	seed := personas.SeedOr(a.Store, personas.Triage)
	for _, g := range groups {
		if !g.Enabled || !triageGroupDue(g, now) {
			continue
		}
		a.runTriageGroup(s, allow, seed, g, now)
	}
}

// runTriageGroup triages one group's sources and stamps its last-run time. allow
// lets triage send the canned auto-reply only to important non-allow-listed
// senders.
func (a *Agent) runTriageGroup(s runner.Settings, allow runner.Allowlist, seed string, g store.TriageGroup, now time.Time) {
	transports := map[string]bool{}
	for _, src := range g.Sources {
		transports[src.Transport] = true
	}
	if len(transports) == 0 {
		return
	}
	triage.RunGroup(a.Client, s, allow, seed, g.Name, transports, personas.Brain(a.Store, personas.Triage))
	_ = a.Store.MarkTriageGroupRan(g.ID, now.UTC().Format(time.RFC3339))
}

// runTriageNow runs every enabled group immediately (the `triage now` command).
func (a *Agent) runTriageNow(s runner.Settings) {
	groups, _ := a.Store.ListTriageGroups()
	allow, _ := a.buildAllow()
	seed := personas.SeedOr(a.Store, personas.Triage)
	now := time.Now()
	ran := false
	for _, g := range groups {
		if g.Enabled {
			a.runTriageGroup(s, allow, seed, g, now)
			ran = true
		}
	}
	if !ran {
		// No groups configured/enabled — fall back to an all-sources pass.
		triage.RunOnce(a.Client, s, allow, seed, personas.Brain(a.Store, personas.Triage))
	}
}

// triageGroupDue reports whether a group should run now given its schedule and
// last run. interval specs ("30m"/"1h"/…) fire every duration; daily specs
// ("09:00,18:00") fire once per listed local time per day.
func triageGroupDue(g store.TriageGroup, now time.Time) bool {
	last := parseRFC3339(g.LastRunAt)
	if g.ScheduleKind == "daily" {
		for _, hm := range strings.Split(g.ScheduleSpec, ",") {
			t, ok := todayAt(strings.TrimSpace(hm), now)
			if !ok {
				continue
			}
			if !now.Before(t) && last.Before(t) {
				return true
			}
		}
		return false
	}
	d := triageInterval(g.ScheduleSpec)
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= d
}

// todayAt returns today's local time at "HH:MM", or ok=false on a bad spec.
func todayAt(hm string, now time.Time) (time.Time, bool) {
	var h, m int
	if _, err := fmt.Sscanf(hm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return time.Time{}, false
	}
	return time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location()), true
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// triageInterval maps a schedule keyword to a poll interval (default 1h).
func triageInterval(schedule string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(schedule)) {
	case "30m":
		return 30 * time.Minute
	case "2h":
		return 2 * time.Hour
	case "3h":
		return 3 * time.Hour
	case "6h":
		return 6 * time.Hour
	case "12h":
		return 12 * time.Hour
	default:
		return time.Hour
	}
}

// subscribe streams inbound 1:1 messages from the comms daemon (all transports)
// and routes each: a chat an active errand owns goes to errands; an
// allow-listed sender goes to the bridge, which replies on the same transport;
// anything else is ignored (comms is a dumb pipe). Own/self messages are dropped
// so the agent never auto-replies to the owner's own outbound.
func (a *Agent) subscribe(ctx context.Context) {
	for ctx.Err() == nil {
		// Context-aware so SIGTERM ends the read at once: a plain Subscribe blocks
		// in its socket read until the daemon closes the connection, which it has
		// no reason to do, so the service used to sit out its whole stop timeout
		// and die on SIGKILL, dropping in-flight work on every deploy.
		err := a.Client.SubscribeContext(ctx, "bridge", func(ev client.Event) {
			if ev.FromSelf {
				return
			}
			if ev.Event == client.EventGroupJoined {
				a.handleGroupJoined(ev)
				return
			}
			// Group calls never reach the 1:1 paths (bridge, errands,
			// reminders, morning, lineup, reschedule).
			if ev.Group {
				a.handleGroupEvent(ev)
				return
			}
			switch transportOf(ev) {
			case "whatsapp":
				// Daemon-delivered WhatsApp (whatsmeow): an errand-owned chat goes
				// to errands, a reminder-owned chat to reminders, else the bridge.
				if a.Errands.OwnsWA(ev.Address) {
					a.Errands.FeedWhatsApp(ev.Address, ev.MsgRef, ev.Text, ev.File)
					return
				}
				if a.Reminders.OwnsWA(ev.Address) {
					a.Reminders.FeedWhatsApp(ev.Address, ev.MsgRef, ev.Text)
					return
				}
				if a.Morning.OwnsWA(ev.Address) {
					a.Morning.FeedWhatsApp(ev.Address, ev.MsgRef, ev.Text)
					return
				}
				if a.Reschedule.OwnsWA(ev.Address) {
					a.Reschedule.FeedWhatsApp(ev.Address, ev.MsgRef, ev.Text)
					return
				}
				a.Bridge.HandleEvent(ev)
			default: // telegram
				if a.Errands.Owns(ev.ChatID) {
					a.Errands.FeedTelegram(ev.ChatID, ev.MessageID, ev.Text, ev.File)
					return
				}
				if a.Reminders.Owns(ev.ChatID) {
					a.Reminders.FeedTelegram(ev.ChatID, ev.MessageID, ev.Text)
					return
				}
				// `remind …` always reaches the reminders module, which passes the
				// whole line to an agent that works out who it is for. Without this
				// an open morning or lineup session ate the keyword and filed the
				// request as one of the owner's own events, so "remind Raani to …"
				// reminded the owner instead of Raani.
				if reminders.IsCommand(ev.Text) {
					a.Bridge.HandleEvent(ev)
					return
				}
				if a.Morning.Owns(ev.ChatID) {
					a.Morning.FeedTelegram(ev.ChatID, ev.MessageID, ev.Text)
					return
				}
				if a.Reschedule.Owns(ev.ChatID) {
					a.Reschedule.FeedTelegram(ev.ChatID, ev.MessageID, ev.Text)
					return
				}
				// The lineup opens on the owner's "lineup" keyword and, once open,
				// takes every owner message until they end it or go quiet.
				if a.Lineup.Trigger(ev.ChatID, ev.MessageID, ev.Text) {
					return
				}
				a.Bridge.HandleEvent(ev)
			}
		})
		if ctx.Err() != nil {
			return
		}
		a.log.Printf("subscription ended (%v); reconnecting…", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// --- agent.sock command server ----------------------------------------------

// sockRequest is one line on agent.sock: a text command (from `zc …`).
type sockRequest struct {
	Text  string `json:"text,omitempty"`
	Actor string `json:"actor,omitempty"`
}

type sockResponse struct {
	OK    bool   `json:"ok"`
	Reply string `json:"reply,omitempty"`
	Error string `json:"error,omitempty"`
}

func (a *Agent) serveCommands(ctx context.Context) {
	dir, err := client.DefaultAppDir()
	if err != nil {
		a.log.Printf("agent.sock: %v", err)
		return
	}
	path := filepath.Join(dir, "agent.sock")
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		a.log.Printf("agent.sock listen: %v", err)
		return
	}
	_ = os.Chmod(path, 0o600)
	go func() { <-ctx.Done(); ln.Close(); os.Remove(path) }()
	a.log.Println("listening on", path)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go a.handleConn(conn)
	}
}

func (a *Agent) handleConn(conn net.Conn) {
	defer conn.Close()
	defer func() {
		if r := recover(); r != nil {
			writeSock(conn, sockResponse{Error: fmt.Sprintf("internal error: %v", r)})
		}
	}()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req sockRequest
	if json.Unmarshal(line, &req) != nil {
		writeSock(conn, sockResponse{Error: "bad request"})
		return
	}
	// A group call's shell must not drive the agent: no errands, reminders,
	// project writes or group changes from inside a group run.
	if peerInGroupCall(conn) || store.CallerFrom(req.Actor) == store.Agent {
		writeSock(conn, sockResponse{Error: "agent commands are not available from a group call"})
		return
	}
	reply, err := a.dispatch(strings.TrimSpace(req.Text))
	if err != nil {
		writeSock(conn, sockResponse{Error: err.Error()})
		return
	}
	writeSock(conn, sockResponse{OK: true, Reply: reply})
}

func writeSock(conn net.Conn, resp sockResponse) {
	b, _ := json.Marshal(resp)
	_, _ = conn.Write(append(b, '\n'))
}

// The comms client is the groups' Admin: the handler finds it by type
// assertion, so this keeps a contract change from silently disabling it.
var _ groups.Admin = (*client.Client)(nil)

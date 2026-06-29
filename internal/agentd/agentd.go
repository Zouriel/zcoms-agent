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
	"time"

	"github.com/Zouriel/zcoms-agent/internal/bootstrap"
	"github.com/Zouriel/zcoms-agent/internal/bridge"
	"github.com/Zouriel/zcoms-agent/internal/errands"
	"github.com/Zouriel/zcoms-agent/internal/personas"
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
	Store    *store.Store
	Registry *workspaces.Registry
	Sessions *sessions.Manager
	Sched    *scheduler.Scheduler
	Client   *client.Client
	Bridge   *bridge.Comp
	Errands  *errands.Comp

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
	agents, err := a.buildAgents()
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

	// seedFn reads a persona's owner-editable seed prompt from agent.db live, so
	// console edits take effect without a restart.
	seedFn := func(key string) string { return personas.SeedOr(a.Store, key) }

	a.Bridge = bridge.New(bridge.Deps{
		Client: a.Client, WAEnabled: settings.WhatsApp.Enabled,
		Locations: locs, Allow: allow, Agents: agents, Settings: settings, MainChatID: mainChat,
		PersonaSeed: seedFn,
	})
	a.Errands = errands.New(a.Client, settings.WhatsApp.Enabled, agents, mainChat, seedFn)

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
	a.Sched.Interval("workspace-discovery", 10*time.Minute, func() { _, _ = a.Registry.Sync() })
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
	now := time.Now()
	seed := personas.SeedOr(a.Store, personas.Triage)
	for _, g := range groups {
		if !g.Enabled || !triageGroupDue(g, now) {
			continue
		}
		a.runTriageGroup(s, seed, g, now)
	}
}

// runTriageGroup triages one group's sources and stamps its last-run time.
func (a *Agent) runTriageGroup(s runner.Settings, seed string, g store.TriageGroup, now time.Time) {
	transports := map[string]bool{}
	for _, src := range g.Sources {
		transports[src.Transport] = true
	}
	if len(transports) == 0 {
		return
	}
	triage.RunGroup(a.Client, s, seed, g.Name, transports)
	_ = a.Store.MarkTriageGroupRan(g.ID, now.UTC().Format(time.RFC3339))
}

// runTriageNow runs every enabled group immediately (the `triage now` command).
func (a *Agent) runTriageNow(s runner.Settings) {
	groups, _ := a.Store.ListTriageGroups()
	seed := personas.SeedOr(a.Store, personas.Triage)
	now := time.Now()
	ran := false
	for _, g := range groups {
		if g.Enabled {
			a.runTriageGroup(s, seed, g, now)
			ran = true
		}
	}
	if !ran {
		// No groups configured/enabled — fall back to an all-sources pass.
		triage.RunOnce(a.Client, s, seed)
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
// and routes each: a chat an active errand/interview owns goes to errands; an
// allow-listed sender goes to the bridge, which replies on the same transport;
// anything else is ignored (comms is a dumb pipe). Own/self messages are dropped
// so the agent never auto-replies to the owner's own outbound.
func (a *Agent) subscribe(ctx context.Context) {
	for ctx.Err() == nil {
		err := a.Client.Subscribe("bridge", func(ev client.Event) {
			if ev.FromSelf {
				return
			}
			switch transportOf(ev) {
			case "whatsapp":
				// Daemon-delivered WhatsApp (whatsmeow): an errand-owned chat goes
				// to errands, everything else to the bridge.
				if a.Errands.OwnsWA(ev.Address) {
					a.Errands.FeedWhatsApp(ev.Address, ev.MsgRef, ev.Text, ev.File)
					return
				}
				a.Bridge.HandleEvent(ev)
			default: // telegram (Instagram joins the bridge path later)
				if a.Errands.Owns(ev.ChatID) {
					a.Errands.FeedTelegram(ev.ChatID, ev.MessageID, ev.Text, ev.File)
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

// sockRequest is one line on agent.sock: a text command (from `zc …`) or a
// standup interview spec (from the team module).
type sockRequest struct {
	Text      string                 `json:"text,omitempty"`
	Actor     string                 `json:"actor,omitempty"`
	Interview *errands.InterviewSpec `json:"interview,omitempty"`
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
	if req.Interview != nil {
		a.Errands.RunInterview(*req.Interview)
		writeSock(conn, sockResponse{OK: true})
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

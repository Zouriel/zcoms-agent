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

	a.Bridge = bridge.New(bridge.Deps{
		Client: a.Client, WASocket: settings.WhatsApp.Socket, WAEnabled: settings.WhatsApp.Enabled,
		Locations: locs, Allow: allow, Agents: agents, Settings: settings, MainChatID: mainChat,
	})
	a.Errands = errands.New(a.Client, settings.WhatsApp.Socket, settings.WhatsApp.Enabled, agents, mainChat)
	return nil
}

// registerJobs wires the scheduler's consumers: triage interval, WhatsApp
// errand-reply poll, due scheduled errands, and periodic workspace discovery.
func (a *Agent) registerJobs() {
	if a.settings.Triage.Enabled {
		d := triageInterval(a.settings.Triage.Schedule)
		a.Sched.Interval("triage", d, func() {
			s, _ := a.buildSettings()
			if s.Triage.Enabled {
				triage.RunOnce(a.Client, s)
			}
		})
	}
	a.Sched.Interval("wa-errand-poll", 25*time.Second, a.Errands.PollWhatsApp)
	a.Sched.Interval("scheduled-errands", 30*time.Second, a.Errands.FireDueScheduled)
	a.Sched.Interval("workspace-discovery", 10*time.Minute, func() { _, _ = a.Registry.Sync() })
}

// runTriageNow runs one triage pass immediately (the `triage now` command).
func (a *Agent) runTriageNow(s runner.Settings) { triage.RunOnce(a.Client, s) }

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

// subscribe streams inbound 1:1 messages from the comms daemon and routes each:
// a chat an active errand/interview owns goes to errands; an allow-listed sender
// goes to the bridge; anything else is ignored (comms is a dumb pipe).
func (a *Agent) subscribe(ctx context.Context) {
	for ctx.Err() == nil {
		err := a.Client.Subscribe("bridge", func(ev client.Event) {
			if a.Errands.Owns(ev.ChatID) {
				a.Errands.FeedTelegram(ev.ChatID, ev.MessageID, ev.Text, ev.File)
				return
			}
			a.Bridge.HandleEvent(ev)
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

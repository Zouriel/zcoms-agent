package bridge

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms/client"
)

// Comp is the bridge component's runtime. It owns the interactive per-user
// session state and reaches Telegram through the core daemon over IPC.
type Comp struct {
	client           *client.Client
	waEnabled        bool
	bridgeBackend    Backend
	chatBackend      Backend
	triageBackend    Backend
	workspaceBackend Backend
	locations        Locations
	allow            Allowlist
	agents           AgentConfig
	settings         Settings
	mainChatID       int64
	personaSeed      func(key string) string

	mu          sync.Mutex
	triageMu    sync.Mutex
	byUser      map[string]*userState // keyed by sessionKey(transport, native id)
	autoReplied map[string]bool       // non-allow-listed senders already sent the canned ack
}

// route is a snapshot of where a session's replies go. It is taken by value
// (st.route()) before any goroutine so an in-flight reply can't race a later
// inbound that mutates the session's address. Every reply goes through the comms
// daemon on the message's own transport.
type route struct {
	transport string // "telegram" | "whatsapp" | "instagram"
	address   string // transport-native reply id (chat id string / JID / thread id)
}

// tgRoute builds a Telegram route from a numeric chat id (for fan-out sends that
// resolve an @handle → chat id, e.g. triage replies).
func tgRoute(chatID int64) route {
	return route{transport: "telegram", address: strconv.FormatInt(chatID, 10)}
}

// waRoute builds a WhatsApp route from a JID (for triage replies to a WA recipient).
func waRoute(jid string) route {
	return route{transport: "whatsapp", address: jid}
}

// seed returns a persona's owner-editable seed prompt, or "" when no accessor is
// wired. Used to prepend the editable scaffold to the chat / workspace first turn.
func (d *Comp) seed(key string) string {
	if d.personaSeed == nil {
		return ""
	}
	return d.personaSeed(key)
}

func (d *Comp) send(r route, text string) { _ = d.sendErr(r, text) }

// sendErr posts text on the route's transport, splitting over the length limit.
// Everything goes through the comms daemon (Send for Telegram, SendOn otherwise).
func (d *Comp) sendErr(r route, text string) error {
	for _, part := range chunk(text, telegramMaxLen) {
		var err error
		switch {
		case r.transport == "" || r.transport == "telegram":
			_, err = d.client.Send(r.address, part)
		default:
			_, err = d.client.SendOn(r.transport, r.address, part)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// sendFile uploads a file on the route's transport, returning the daemon's label.
func (d *Comp) sendFile(r route, path, caption string) (string, error) {
	switch {
	case r.transport == "" || r.transport == "telegram":
		resp, err := d.client.SendFile(r.address, path, caption)
		return resp.Label, err
	default:
		resp, err := d.client.SendFileOn(r.transport, r.address, path, caption)
		return resp.Label, err
	}
}

func (d *Comp) resolveChat(target string) (int64, int64, error) {
	id, err := d.client.Resolve(target)
	return id, id, err
}

func (d *Comp) currentTriage() TriageSettings {
	if s, _, err := runner.LoadOrSeedSettings(); err == nil {
		return s.Triage
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.settings.Triage
}

// errandCommand forwards an `errand …` command to the errands component over
// errands.sock and returns its reply (or a clear error string).
func (d *Comp) errandCommand(text string) string {
	dir, err := runner.DefaultAppDir()
	if err != nil {
		return "⚠️ " + err.Error()
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "errands.sock"), 2*time.Second)
	if err != nil {
		return "The errands component isn't running — install it with `zc install errands`."
	}
	defer conn.Close()
	req, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{text})
	_, _ = conn.Write(append(req, '\n'))
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, _ := bufio.NewReader(conn).ReadBytes('\n')
	var resp struct {
		OK    bool   `json:"ok"`
		Reply string `json:"reply"`
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &resp) != nil {
		return "⚠️ couldn't reach the errands component"
	}
	if !resp.OK {
		return "⚠️ " + resp.Error
	}
	return resp.Reply
}

// handleErrandCommand relays an `errand …` bridge command to the errands component.
func (d *Comp) handleErrandCommand(st *userState, text string) {
	d.send(st.route(), d.errandCommand(text))
}

// isTeamCommand reports whether a message should be routed to the zc-team
// component (lowercased text).
func isTeamCommand(lower string) bool {
	switch lower {
	case "add task", "add tasks", "new task", "finish task", "team":
		return true
	}
	for _, p := range []string{"team ", "delegator ", "standup ", "staff ", "task ", "agent create "} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// handleTeamCommand forwards a message to the team component over team.sock and
// relays the reply, staying in a "team session" while the component asks for more
// (multi-turn flows like add/new/finish task).
func (d *Comp) handleTeamCommand(st *userState, text string) {
	dir, err := runner.DefaultAppDir()
	if err != nil {
		d.send(st.route(), "⚠️ "+err.Error())
		return
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "team.sock"), 2*time.Second)
	if err != nil {
		d.setTeamSession(st, false)
		d.send(st.route(), "The team component isn't running — install it with `zc install team`.")
		return
	}
	defer conn.Close()
	req, _ := json.Marshal(struct {
		Text  string `json:"text"`
		Actor string `json:"actor"`
	}{text, st.username})
	_, _ = conn.Write(append(req, '\n'))
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	line, _ := bufio.NewReader(conn).ReadBytes('\n')
	var resp struct {
		OK       bool   `json:"ok"`
		Reply    string `json:"reply"`
		Continue bool   `json:"continue"`
		Error    string `json:"error"`
	}
	if json.Unmarshal(line, &resp) != nil {
		d.setTeamSession(st, false)
		d.send(st.route(), "⚠️ couldn't reach the team component")
		return
	}
	d.setTeamSession(st, resp.Continue)
	if !resp.OK {
		d.send(st.route(), "⚠️ "+resp.Error)
		return
	}
	d.send(st.route(), resp.Reply)
}

func (d *Comp) setTeamSession(st *userState, on bool) {
	d.mu.Lock()
	st.teamSession = on
	d.mu.Unlock()
}

// isCommerceCommand reports whether a message should be routed to the
// zc-commerce component (lowercased text).
func isCommerceCommand(lower string) bool {
	return lower == "commerce" || strings.HasPrefix(lower, "commerce ")
}

// handleCommerceCommand forwards a `commerce …` message to the commerce
// component over commerce.sock and relays the reply. Unlike team, commerce is
// stateless — one request, one response.
func (d *Comp) handleCommerceCommand(st *userState, text string) {
	// Strip the leading "commerce" so the component sees its own subcommand
	// (e.g. "commerce store list" -> "store list"), matching the CLI which
	// passes only the args after `zc commerce`.
	sub := strings.TrimSpace(strings.TrimPrefix(text, "commerce"))

	dir, err := runner.DefaultAppDir()
	if err != nil {
		d.send(st.route(), "⚠️ "+err.Error())
		return
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "commerce.sock"), 2*time.Second)
	if err != nil {
		d.send(st.route(), "The commerce component isn't running — install it with `zc install commerce`.")
		return
	}
	defer conn.Close()
	req, _ := json.Marshal(struct {
		Text  string `json:"text"`
		Actor string `json:"actor"`
	}{sub, st.username})
	_, _ = conn.Write(append(req, '\n'))
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	line, _ := bufio.NewReader(conn).ReadBytes('\n')
	var resp struct {
		OK    bool   `json:"ok"`
		Reply string `json:"reply"`
		Error string `json:"error"`
	}
	if json.Unmarshal(line, &resp) != nil {
		d.send(st.route(), "⚠️ couldn't reach the commerce component")
		return
	}
	if !resp.OK {
		d.send(st.route(), "⚠️ "+resp.Error)
		return
	}
	d.send(st.route(), resp.Reply)
}

// triage-session.json helpers (the bridge resumes/resets the shared triage brain
// the same way the daemon and triage component do).
type triageSession struct {
	SessionID string    `json:"session_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

func LoadTriageSessionID() (string, error) {
	dir, err := runner.DefaultAppDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "triage-session.json"))
	if err != nil {
		return "", nil
	}
	var s triageSession
	if json.Unmarshal(data, &s) != nil {
		return "", nil
	}
	return s.SessionID, nil
}

func SaveTriageSessionID(id string) error {
	if id == "" {
		return nil
	}
	dir, err := runner.DefaultAppDir()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "triage-session.json"), triageSession{SessionID: id, UpdatedAt: time.Now()})
}

func ResetTriageSession() error {
	dir, err := runner.DefaultAppDir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, "triage-session.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

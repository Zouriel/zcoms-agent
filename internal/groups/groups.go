// Package groups answers calls in group chats. The comms daemon forwards only
// calls (an @mention of the agent's account or a reply to one of its messages)
// from groups the owner turned on; this package looks up the group's policy,
// fetches the window around the call, runs one fresh, tool-restricted model
// turn, and replies to the calling message in the same group.
//
// Safety lives in code, not in the prompt: the reply target is always the
// calling group and message, never anything the model wrote; a group with no
// policy row is dropped before anything is read; calls are queued and rate
// limited per group; and a group run never gets edit or write tools.
package groups

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
	"github.com/Zouriel/zcoms/client"
)

// Limits on group calls.
const (
	MaxQueued   = 3   // calls waiting behind the one running, per group
	MaxPerHour  = 20  // admitted calls per group per rolling hour
	MaxReply    = 700 // characters in one group reply
	WindowCount = 50  // messages before and after the call
)

// Persona is the persona key group answers run on.
const Persona = "group"

// FailReply is sent when a call cannot be answered.
const FailReply = "Sorry, I could not answer that one right now."

// Comms is the slice of the comms client the handler uses. It must be an
// agent-caller client, so the daemon applies its own group_read limits.
type Comms interface {
	GroupRead(transport, group string, o client.GroupReadOpts) ([]client.GroupMessage, error)
	SendOpts(transport, to, text, replyTo string, mentions []string) (client.Response, error)
}

// Policies reads group policies from agent.db.
type Policies interface {
	GetGroupPolicy(transport, nativeID string) (store.GroupPolicy, bool, error)
	TouchGroupCall(transport, nativeID string, at time.Time) error
}

// RunRequest is one restricted model turn.
type RunRequest struct {
	Persona string
	Dir     string
	Prompt  string
	Profile runner.ToolProfile
}

// Runner runs one model turn and returns its text.
type Runner func(RunRequest) (string, error)

// Call is one group call as the handler sees it.
type Call struct {
	Transport  string
	GroupID    string // the group's native id; replies go here and only here
	GroupName  string
	MsgID      string // the calling message; replies quote it
	SenderID   string
	SenderName string
	Text       string
	Kind       string // "mention" | "reply"
	ReplyToID  string // the message the call replied to, if any
}

// ProjectContext is what a project-group call runs with.
type ProjectContext struct {
	Dir     string             // the bound folder
	Profile runner.ToolProfile // read, search, maybe shell; never edit
	Notes   string             // recent summaries, open tasks, history db path
}

// Projects is the project side of group calls.
type Projects interface {
	// CallContext returns the folder, tools and notes for a project call.
	CallContext(projectID int64) (ProjectContext, error)
	// StandupReply handles a reply to a standup message. handled=false means
	// it was an ordinary question and should be answered normally.
	StandupReply(projectID int64, c Call) (reply string, handled bool)
}

// Handler answers group calls.
type Handler struct {
	Comms    Comms
	Policies Policies
	Run      Runner
	Projects Projects // nil: project-group calls get the fail reply
	Seed     func(key string) string
	Scratch  string // parent of the per-call empty scratch dirs
	Log      *log.Logger
	Now      func() time.Time

	mu     sync.Mutex
	groups map[string]*groupState
}

type groupState struct {
	busy   bool
	queue  []Call
	stamps []time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log.Printf(format, args...)
	}
}

// CallFromEvent converts a subscription event to a Call. ok is false for
// anything that is not a group call.
func CallFromEvent(ev client.Event) (Call, bool) {
	if !ev.Group || ev.FromSelf {
		return Call{}, false
	}
	tp := ev.Transport
	if tp == "" {
		tp = "telegram"
	}
	gid := ev.Address
	if gid == "" && ev.ChatID != 0 {
		gid = strconv.FormatInt(ev.ChatID, 10)
	}
	mid := ev.MsgRef
	if mid == "" && ev.MessageID != 0 {
		mid = strconv.FormatInt(ev.MessageID, 10)
	}
	if gid == "" || mid == "" {
		return Call{}, false
	}
	return Call{
		Transport: tp, GroupID: gid, GroupName: ev.GroupName, MsgID: mid,
		SenderID: ev.SenderID, SenderName: ev.Sender, Text: ev.Text,
		Kind: ev.Call, ReplyToID: ev.ReplyToID,
	}, true
}

// Handle admits one event. It returns at once; the answer is produced on the
// group's worker goroutine, which exits when the group's queue is empty.
func (h *Handler) Handle(ev client.Event) {
	c, ok := CallFromEvent(ev)
	if !ok {
		return
	}
	_, ok, err := h.Policies.GetGroupPolicy(c.Transport, c.GroupID)
	if err != nil {
		h.logf("group policy lookup %s/%s: %v", c.Transport, c.GroupID, err)
		return
	}
	if !ok {
		return // off: no reply, no reads
	}
	start, admitted := h.admit(c)
	if !admitted {
		h.logf("group %s/%s: call %s dropped (queue full or hourly limit)", c.Transport, c.GroupID, c.MsgID)
		return
	}
	if start {
		go h.work(c)
	}
}

// admit applies the per-group queue and hourly limit. start reports that no
// worker is running for the group, so the caller must start one for c.
func (h *Handler) admit(c Call) (start, admitted bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.groups == nil {
		h.groups = map[string]*groupState{}
	}
	key := c.Transport + "|" + c.GroupID
	g := h.groups[key]
	if g == nil {
		g = &groupState{}
		h.groups[key] = g
	}
	now := h.now()
	kept := g.stamps[:0]
	for _, t := range g.stamps {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	g.stamps = kept
	if len(g.stamps) >= MaxPerHour {
		return false, false
	}
	if g.busy {
		if len(g.queue) >= MaxQueued {
			return false, false
		}
		g.queue = append(g.queue, c)
		g.stamps = append(g.stamps, now)
		return false, true
	}
	g.busy = true
	g.stamps = append(g.stamps, now)
	return true, true
}

// next pops the group's next queued call, or marks the group idle.
func (h *Handler) next(c Call) (Call, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.groups[c.Transport+"|"+c.GroupID]
	if g == nil || len(g.queue) == 0 {
		if g != nil {
			g.busy = false
		}
		return Call{}, false
	}
	n := g.queue[0]
	g.queue = g.queue[1:]
	return n, true
}

func (h *Handler) work(c Call) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					h.logf("group call panic: %v", r)
				}
			}()
			// Re-read the policy: the owner may have turned the group off
			// while this call waited.
			if p, ok, err := h.Policies.GetGroupPolicy(c.Transport, c.GroupID); err == nil && ok {
				h.Process(p, c)
			}
		}()
		var more bool
		if c, more = h.next(c); !more {
			return
		}
	}
}

// Process answers one admitted call synchronously. Exported for tests.
func (h *Handler) Process(pol store.GroupPolicy, c Call) {
	_ = h.Policies.TouchGroupCall(c.Transport, c.GroupID, h.now())
	if pol.Kind == store.GroupProject && h.Projects != nil && c.Kind == "reply" && c.ReplyToID != "" {
		if reply, handled := h.Projects.StandupReply(pol.ProjectID, c); handled {
			if reply != "" {
				h.reply(c, reply)
			}
			return
		}
	}
	text, err := h.answer(pol, c)
	if err != nil {
		h.logf("group %s/%s: answer failed: %v", c.Transport, c.GroupID, err)
		text = FailReply
	}
	h.reply(c, text)
}

// reply sends text to the calling group, quoting the calling message. The
// target comes from the call, never from model output.
func (h *Handler) reply(c Call, text string) {
	if _, err := h.Comms.SendOpts(c.Transport, c.GroupID, text, c.MsgID, nil); err != nil {
		h.logf("group %s/%s: send failed: %v", c.Transport, c.GroupID, err)
	}
}

func (h *Handler) answer(pol store.GroupPolicy, c Call) (string, error) {
	window, err := h.Comms.GroupRead(c.Transport, c.GroupID, client.GroupReadOpts{Around: c.MsgID, Limit: WindowCount})
	if err != nil {
		h.logf("group %s/%s: window read: %v", c.Transport, c.GroupID, err)
		window = nil
	}
	var (
		dir     string
		profile runner.ToolProfile
		kind    string
		cleanup func()
	)
	switch pol.Kind {
	case store.GroupProject:
		if h.Projects == nil {
			return "", fmt.Errorf("project groups are not available")
		}
		pc, err := h.Projects.CallContext(pol.ProjectID)
		if err != nil {
			return "", err
		}
		dir, profile = pc.Dir, pc.Profile
		kind = projectKind(pc)
	default:
		d, err := os.MkdirTemp(h.Scratch, "group-call-*")
		if err != nil {
			return "", err
		}
		dir, cleanup = d, func() { _ = os.RemoveAll(d) }
		profile = GeneralProfile
		kind = generalKind
	}
	if cleanup != nil {
		defer cleanup()
	}
	prompt := BuildPrompt(h.seed(), c, window, kind)
	out, err := h.Run(RunRequest{Persona: Persona, Dir: dir, Prompt: prompt, Profile: profile})
	if err != nil {
		return "", err
	}
	return h.capReply(dir, strings.TrimSpace(out)), nil
}

// GeneralProfile is the only reach a general group call has: web search and
// fetch, in an empty scratch dir.
var GeneralProfile = runner.ToolProfile{Tools: []string{"WebSearch", "WebFetch"}}

func (h *Handler) seed() string {
	if h.Seed == nil {
		return ""
	}
	return h.Seed(Persona)
}

// capReply keeps a reply within MaxReply: one request to shorten, then a cut
// at the last full sentence.
func (h *Handler) capReply(dir, text string) string {
	if text == "" {
		return FailReply
	}
	if len([]rune(text)) <= MaxReply {
		return text
	}
	short, err := h.Run(RunRequest{
		Persona: Persona, Dir: dir,
		Prompt: fmt.Sprintf("Shorten this group chat reply to under %d characters. Keep its meaning and plain tone. Reply with only the shortened text.\n\n%s", MaxReply, text),
	})
	if err == nil && strings.TrimSpace(short) != "" {
		text = strings.TrimSpace(short)
	}
	return CutAtSentence(text, MaxReply)
}

// CutAtSentence trims s to at most max characters, ending at the last full
// sentence when there is one.
func CutAtSentence(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max])
	best := -1
	for _, end := range []string{". ", "! ", "? ", ".\n", "!\n", "?\n"} {
		if i := strings.LastIndex(cut, end); i > best {
			best = i
		}
	}
	for _, end := range []string{".", "!", "?"} {
		if strings.HasSuffix(cut, end) {
			return cut
		}
	}
	if best > 0 {
		return cut[:best+1]
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

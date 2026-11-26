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

// Limits on group calls. Every call gets its own independent run, so many
// questions are answered at once; MaxConcurrent is a safety ceiling across all
// groups, and a call that cannot get a slot within SlotWait gets FailReply.
const (
	MaxConcurrent = 8
	SlotWait      = 5 * time.Minute
	MaxPerHour    = 20  // admitted calls per group per rolling hour
	MaxReply      = 700 // characters in one group reply
	WindowCount   = 50  // messages before and after the call
)

// Persona keys. Every call runs two passes: UnderstandPersona (the backend's
// top model, no tools, tiny output) restates the call as a self-contained
// English task; Persona (a lower model, with the group's tools) does the work
// and writes the reply.
const (
	Persona           = "group"
	UnderstandPersona = "group_understand"
)

// understandWindow is how many messages before the call the understand pass
// sees; it only needs enough to resolve "it" and "that".
const understandWindow = 10

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
	// File and ReplyFile are local paths (owned by zcoms) of the calling
	// message's media and of the quoted message's media. Both passes may read
	// exactly these files and nothing else from the group.
	File      string
	ReplyFile string
}

// Attachments returns the call's readable media files: the calling message's
// own media, then the quoted message's. Paths a read rule cannot pin to one
// file are dropped.
func (c Call) Attachments() []string {
	var out []string
	for _, f := range []string{c.File, c.ReplyFile} {
		if f == "" || containsPath(out, f) || len(runner.FileReadRules([]string{f})) == 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}

func containsPath(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ProjectContext is what a project-group call runs with.
type ProjectContext struct {
	Dir     string             // the bound folder
	Profile runner.ToolProfile // read, search, maybe shell; never edit
	Notes   string             // recent summaries, open tasks, history db path
	MaxRole string             // the workspace ceiling the profile was built under
	// General is set for a tracked group whose folder holds no repo: the call
	// runs like a general group (web only, empty scratch dir) with Notes added.
	General bool
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
	stamps map[string][]time.Time // admitted calls per group, last hour

	semOnce sync.Once
	sem     chan struct{}
}

func (h *Handler) slots() chan struct{} {
	h.semOnce.Do(func() { h.sem = make(chan struct{}, MaxConcurrent) })
	return h.sem
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
		File: ev.File, ReplyFile: ev.ReplyFile,
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
	if !h.admit(c) {
		h.logf("group %s/%s: call %s dropped (hourly limit)", c.Transport, c.GroupID, c.MsgID)
		return
	}
	go h.work(c)
}

// admit applies the per-group hourly limit.
func (h *Handler) admit(c Call) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stamps == nil {
		h.stamps = map[string][]time.Time{}
	}
	key := c.Transport + "|" + c.GroupID
	now := h.now()
	var kept []time.Time
	for _, t := range h.stamps[key] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= MaxPerHour {
		h.stamps[key] = kept
		return false
	}
	h.stamps[key] = append(kept, now)
	return true
}

// work runs one call on its own goroutine once a concurrency slot is free.
// It exits when the call is answered, or after SlotWait with FailReply.
func (h *Handler) work(c Call) {
	select {
	case h.slots() <- struct{}{}:
	case <-time.After(SlotWait):
		h.logf("group %s/%s: no free slot for call %s", c.Transport, c.GroupID, c.MsgID)
		h.reply(c, FailReply)
		return
	}
	defer func() { <-h.slots() }()
	defer func() {
		if r := recover(); r != nil {
			h.logf("group call panic: %v", r)
		}
	}()
	// Re-read the policy: the owner may have turned the group off while this
	// call waited for a slot.
	if p, ok, err := h.Policies.GetGroupPolicy(c.Transport, c.GroupID); err == nil && ok {
		h.Process(p, c)
	}
}

// Process answers one admitted call synchronously. Exported for tests.
func (h *Handler) Process(pol store.GroupPolicy, c Call) {
	_ = h.Policies.TouchGroupCall(c.Transport, c.GroupID, h.now())
	// Any call quoting a message may be a standup reply, including one that
	// also @mentions the agent (Kind "mention"); StandupReply checks the id.
	if pol.Kind == store.GroupProject && h.Projects != nil && c.ReplyToID != "" {
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
		if pc.General {
			d, err := os.MkdirTemp(h.Scratch, "group-call-*")
			if err != nil {
				return "", err
			}
			dir, cleanup = d, func() { _ = os.RemoveAll(d) }
			profile = GeneralProfile
			kind = tasksKind(pc)
			break
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
	// Attached media rides on top of the group's normal tools; a fresh slice
	// so a shared profile (GeneralProfile) is never mutated.
	if files := c.Attachments(); len(files) > 0 {
		profile.Files = append(append([]string{}, profile.Files...), files...)
	}
	task, lang := h.understand(c, window)
	prompt := BuildPrompt(h.seed(Persona), c, task, lang, window, kind)
	out, err := h.Run(RunRequest{Persona: Persona, Dir: dir, Prompt: prompt, Profile: profile})
	if err != nil {
		return "", err
	}
	return h.capReply(dir, strings.TrimSpace(out)), nil
}

// GeneralProfile is the only reach a general group call has: web search and
// fetch, in an empty scratch dir.
var GeneralProfile = runner.ToolProfile{Tools: []string{"WebSearch", "WebFetch"}}

func (h *Handler) seed(key string) string {
	if h.Seed == nil {
		return ""
	}
	return h.Seed(key)
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

// understand runs the tool-less first pass and returns the English task and
// the asker's language. Without a model answer it falls back to the raw text.
func (h *Handler) understand(c Call, window []client.GroupMessage) (task, lang string) {
	task = strings.TrimSpace(c.Text)
	dir, err := os.MkdirTemp(h.Scratch, "group-understand-*")
	if err != nil {
		return task, ""
	}
	defer os.RemoveAll(dir)
	// No tools, except reading exactly the caller's attached files.
	out, err := h.Run(RunRequest{Persona: UnderstandPersona, Dir: dir, Prompt: UnderstandPrompt(h.seed(UnderstandPersona), c, window),
		Profile: runner.ToolProfile{Files: c.Attachments()}})
	if err != nil {
		h.logf("group %s/%s: understand pass: %v", c.Transport, c.GroupID, err)
		return task, ""
	}
	t, l, none := ParseUnderstand(out)
	switch {
	case none != "":
		return "The caller did not ask a question (" + none + "). Reply with one short, friendly line.", l
	case t != "":
		return t, l
	}
	return task, l
}

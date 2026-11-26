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
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	SendWith(transport, to, text string, o client.SendOptions) (client.Response, error)
	SendFileWith(transport, to, path, caption string, o client.SendOptions) (client.Response, error)
	TopicCreate(transport, group, name string) (*client.Topic, error)
	TopicList(transport, group string) ([]client.Topic, error)
}

// Policies reads group policies from agent.db.
type Policies interface {
	GetGroupPolicy(transport, nativeID string) (store.GroupPolicy, bool, error)
	TouchGroupCall(transport, nativeID string, at time.Time) error
	// GroupRole is a member's role in a group (store.RoleRead by default).
	GroupRole(transport, nativeID, memberID string) (string, error)
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

// Transcriber turns one audio file into the words spoken in it.
type Transcriber func(path string) (string, error)

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
	// ThreadID is the thread (Telegram forum topic) the call was made in;
	// replies go back into it. Empty outside a thread.
	ThreadID string
	// File and ReplyFile are local paths (owned by zcoms) of the calling
	// message's media and of the quoted message's media. Both passes may read
	// exactly these files and nothing else from the group.
	File      string
	ReplyFile string
	// SenderIsOwner comes from the comms daemon (the sender resolved to the
	// configured owner), never from message text.
	SenderIsOwner bool
	// Voice holds the transcripts of the call's audio files, filled in before
	// the understand pass. Audio is never attached as a file: the model
	// passes cannot hear it.
	Voice []VoiceNote
}

// VoiceNote is one audio attachment after transcription.
type VoiceNote struct {
	File string // the audio file, for saying which message it came from
	Text string // the transcript; empty when it could not be transcribed
}

// Attachments returns the call's readable non-audio media files: the calling
// message's own media, then the quoted message's. Paths a read rule cannot
// pin to one file are dropped. Audio goes through AudioFiles instead.
func (c Call) Attachments() []string {
	var out []string
	for _, f := range c.media() {
		if !IsAudio(f) && len(runner.FileReadRules([]string{f})) > 0 {
			out = append(out, f)
		}
	}
	return out
}

// AudioFiles returns the call's audio attachments (voice notes, audio
// files), which are transcribed rather than attached.
func (c Call) AudioFiles() []string {
	var out []string
	for _, f := range c.media() {
		if IsAudio(f) {
			out = append(out, f)
		}
	}
	return out
}

// media lists the call's distinct media paths, calling message first.
func (c Call) media() []string {
	var out []string
	for _, f := range []string{c.File, c.ReplyFile} {
		if f != "" && !containsPath(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// audioExts are the extensions treated as audio without looking inside.
var audioExts = map[string]bool{
	".ogg": true, ".oga": true, ".opus": true, ".mp3": true, ".m4a": true,
	".wav": true, ".aac": true, ".webm": true, ".flac": true, ".amr": true,
}

// IsAudio reports whether path is an audio file: by extension, else by
// sniffing its first bytes.
func IsAudio(path string) bool {
	if audioExts[strings.ToLower(filepath.Ext(path))] {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ct := http.DetectContentType(head[:n])
	return ct == "application/ogg" || strings.HasPrefix(ct, "audio/")
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
	// CanWrite says the ceiling allows edits (max_role edit, full or none), so
	// a write member's call may get the edit tools.
	CanWrite bool
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
	// Transcribe turns voice notes into text before the understand pass.
	// nil, or a failure, leaves a note asking the caller to type instead.
	Transcribe Transcriber
	Projects   Projects // nil: project-group calls get the fail reply
	Seed       func(key string) string
	Scratch    string // parent of the per-call empty scratch dirs
	Log        *log.Logger
	Now        func() time.Time

	mu     sync.Mutex
	stamps map[string][]time.Time // admitted calls per group, last hour
	topics topicLimiter           // new threads per group, last hour

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
		Kind: ev.Call, ReplyToID: ev.ReplyToID, ThreadID: ev.ThreadID,
		File: ev.File, ReplyFile: ev.ReplyFile,
		SenderIsOwner: ev.SenderIsOwner,
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
	// Every call gets its own empty outbox; the model may leave files there
	// and list them with ATTACH lines. It is removed once the call is done.
	outbox, err := os.MkdirTemp(h.Scratch, "group-outbox-*")
	if err != nil {
		h.logf("group %s/%s: outbox: %v", c.Transport, c.GroupID, err)
		outbox = ""
	} else {
		defer os.RemoveAll(outbox)
	}
	ans, err := h.answer(pol, c, outbox)
	if err != nil {
		h.logf("group %s/%s: answer failed: %v", c.Transport, c.GroupID, err)
		h.reply(c, FailReply)
		return
	}
	h.deliver(c, ans, outbox)
}

// answered is a work pass's result: the reply text, the code-handled
// directives, and the project folder ATTACH paths may come from ("" for a
// general group).
type answered struct {
	Text       string
	Directives Directives
	ProjectDir string
}

// deliver posts an answer. Everything goes to the calling group only: the
// group id comes from the call, never from model output. A THREAD request
// creates at most one thread, in the calling group, and posts the answer
// there with a one-line pointer quoting the call; files follow the answer.
func (h *Handler) deliver(c Call, a answered, outbox string) {
	files, skipped := ValidateAttachments(a.Directives.Attach, outbox, a.ProjectDir)
	for _, s := range skipped {
		h.logf("group %s/%s: call %s: attachment %q skipped: %s", c.Transport, c.GroupID, c.MsgID, s.Path, s.Reason)
	}
	text := a.Text
	// A call made inside a thread is answered there; it never starts another.
	if name := a.Directives.Thread; name != "" && c.ThreadID == "" {
		tid, note := h.startThread(c, name)
		if tid != "" {
			first := text
			if _, err := h.Comms.SendWith(c.Transport, c.GroupID, first, client.SendOptions{Thread: tid}); err != nil {
				h.logf("group %s/%s: send in new thread failed: %v", c.Transport, c.GroupID, err)
				h.reply(c, text)
				h.sendFiles(c, files, c.ThreadID, c.MsgID)
				return
			}
			h.reply(c, fmt.Sprintf("I started a thread for this: %s.", name))
			h.sendFiles(c, files, tid, "")
			return
		}
		text = strings.TrimSpace(text + " " + note)
	}
	h.reply(c, text)
	h.sendFiles(c, files, c.ThreadID, c.MsgID)
}

// startThread creates one thread named name in the calling group. It returns
// the thread id, or "" and a short clause saying why the answer stays here.
func (h *Handler) startThread(c Call, name string) (string, string) {
	// Reuse a thread with the same name (case-insensitive) instead of adding a
	// near-duplicate topic; only a genuinely new name creates one.
	if ts, err := h.Comms.TopicList(c.Transport, c.GroupID); err == nil {
		for _, t := range ts {
			if strings.EqualFold(strings.TrimSpace(t.Name), strings.TrimSpace(name)) && t.ID != "" {
				return t.ID, ""
			}
		}
	}
	if !h.topics.allow(c.Transport+"|"+c.GroupID, h.now()) {
		return "", "(No new thread: too many started this hour.)"
	}
	t, err := h.Comms.TopicCreate(c.Transport, c.GroupID, name)
	if err == nil && t != nil && t.ID != "" {
		return t.ID, ""
	}
	h.logf("group %s/%s: thread %q not started: %v", c.Transport, c.GroupID, name, err)
	msg := ""
	if err != nil {
		msg = strings.ToLower(err.Error())
	}
	switch {
	case strings.Contains(msg, "do not have threads"):
		return "", "(WhatsApp groups have no threads, so I answered here.)"
	case strings.Contains(msg, "topics are off"):
		return "", "(I could not start a thread: topics are off in this group.)"
	}
	return "", "(I could not start a thread, so I answered here.)"
}

// sendFiles sends validated files to the calling group, quoting replyTo and
// in thread when set. A failure is logged and the rest still go.
func (h *Handler) sendFiles(c Call, files []string, thread, replyTo string) {
	for _, f := range files {
		if _, err := h.Comms.SendFileWith(c.Transport, c.GroupID, f, "", client.SendOptions{ReplyTo: replyTo, Thread: thread}); err != nil {
			h.logf("group %s/%s: sending %s failed: %v", c.Transport, c.GroupID, f, err)
		}
	}
}

// reply sends text to the calling group, quoting the calling message, in
// the thread the call came from. The target comes from the call, never from
// model output.
func (h *Handler) reply(c Call, text string) {
	if _, err := h.Comms.SendWith(c.Transport, c.GroupID, text, client.SendOptions{ReplyTo: c.MsgID, Thread: c.ThreadID}); err != nil {
		h.logf("group %s/%s: send failed: %v", c.Transport, c.GroupID, err)
	}
}

func (h *Handler) answer(pol store.GroupPolicy, c Call, outbox string) (answered, error) {
	var a answered
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
		write   bool
		// denied is the fixed reply for a change request this caller may
		// not make; decided here in code, never by a model turn.
		denied = generalDenial
	)
	switch pol.Kind {
	case store.GroupProject:
		if h.Projects == nil {
			return a, fmt.Errorf("project groups are not available")
		}
		pc, err := h.Projects.CallContext(pol.ProjectID)
		if err != nil {
			return a, err
		}
		if pc.General {
			d, err := os.MkdirTemp(h.Scratch, "group-call-*")
			if err != nil {
				return a, err
			}
			dir, cleanup = d, func() { _ = os.RemoveAll(d) }
			profile = GeneralProfile
			kind = tasksKind(pc)
			break
		}
		dir, profile = pc.Dir, pc.Profile
		write = pc.CanWrite && h.memberRole(c) == store.RoleWrite
		if !write {
			denied = readOnlyDenial
			if !pc.CanWrite {
				denied = projectReadOnlyDenial
			}
		}
		if write {
			profile = WithEdits(profile)
		}
		kind = projectKind(pc, write)
		a.ProjectDir = pc.Dir
		kind += "\n\n" + shareNote(outbox, profile.HasShell())
	default:
		d, err := os.MkdirTemp(h.Scratch, "group-call-*")
		if err != nil {
			return a, err
		}
		dir, cleanup = d, func() { _ = os.RemoveAll(d) }
		profile = GeneralProfile
		kind = generalKind
	}
	if cleanup != nil {
		defer cleanup()
	}
	c.Voice = h.transcribe(c)
	// Attached media rides on top of the group's normal tools; a fresh slice
	// so a shared profile (GeneralProfile) is never mutated.
	if files := c.Attachments(); len(files) > 0 {
		profile.Files = append(append([]string{}, profile.Files...), files...)
	}
	u := h.understand(c, window)
	if u.Changes && !write {
		// Refuse before any acknowledgement, so the asker is never told the
		// work has started.
		a.Text = denied
		return a, nil
	}
	task, lang, ack := u.Task, u.Lang, u.Ack
	if ack != "" {
		// Long work: tell the asker it has started, so the wait is not silent.
		h.reply(c, ack)
	}
	prompt := BuildPrompt(h.seed(Persona), c, task, lang, window, kind)
	out, err := h.Run(RunRequest{Persona: Persona, Dir: dir, Prompt: prompt, Profile: profile})
	if err != nil {
		return a, err
	}
	text, d := SplitDirectives(strings.TrimSpace(out))
	if text == "" && len(d.Attach) > 0 {
		text = "Here it is."
	}
	a.Text, a.Directives = h.capReply(dir, text), d
	return a, nil
}

// transcribe turns the call's audio files into transcripts. A failure leaves
// an empty transcript (the prompts then ask the caller to type) and is logged
// once for the call.
func (h *Handler) transcribe(c Call) []VoiceNote {
	files := c.AudioFiles()
	if len(files) == 0 {
		return nil
	}
	notes := make([]VoiceNote, 0, len(files))
	var failed []string
	for _, f := range files {
		n := VoiceNote{File: f}
		switch {
		case h.Transcribe == nil:
			failed = append(failed, "no transcriber configured")
		default:
			text, err := h.Transcribe(f)
			if err != nil {
				failed = append(failed, err.Error())
			} else {
				n.Text = strings.TrimSpace(text)
			}
		}
		notes = append(notes, n)
	}
	if len(failed) > 0 {
		h.logf("group %s/%s: call %s: voice note not transcribed: %s", c.Transport, c.GroupID, c.MsgID, strings.Join(failed, "; "))
	}
	return notes
}

// EditTools are what the write role adds in a project group.
var EditTools = []string{"Edit", "Write", "MultiEdit"}

// WithEdits returns a copy of p with the edit tools added.
func WithEdits(p runner.ToolProfile) runner.ToolProfile {
	out := p
	out.Tools = append(append([]string{}, p.Tools...), EditTools...)
	return out
}

// memberRole decides the caller's role in code, before the runner starts:
// the owner (as marked by the comms daemon) is always write; anyone else has
// the role the owner stored for them, read by default. A lookup error is read.
func (h *Handler) memberRole(c Call) string {
	if c.SenderIsOwner {
		return store.RoleWrite
	}
	if c.SenderID == "" {
		return store.RoleRead
	}
	r, err := h.Policies.GroupRole(c.Transport, c.GroupID, c.SenderID)
	if err != nil {
		h.logf("group %s/%s: role lookup: %v", c.Transport, c.GroupID, err)
		return store.RoleRead
	}
	return r
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
func (h *Handler) understand(c Call, window []client.GroupMessage) Understood {
	fallback := Understood{Task: strings.TrimSpace(c.Text)}
	dir, err := os.MkdirTemp(h.Scratch, "group-understand-*")
	if err != nil {
		return fallback
	}
	defer os.RemoveAll(dir)
	// No tools, except reading exactly the caller's attached files.
	out, err := h.Run(RunRequest{Persona: UnderstandPersona, Dir: dir, Prompt: UnderstandPrompt(h.seed(UnderstandPersona), c, window),
		Profile: runner.ToolProfile{Files: c.Attachments()}})
	if err != nil {
		h.logf("group %s/%s: understand pass: %v", c.Transport, c.GroupID, err)
		return fallback
	}
	u := ParseUnderstood(out)
	switch {
	case u.None != "":
		return Understood{Task: "The caller did not ask a question (" + u.None + "). Reply with one short, friendly line.", Lang: u.Lang}
	case u.Task != "":
		return u
	}
	return Understood{Task: fallback.Task, Lang: u.Lang}
}

// The fixed replies to a change request the caller may not make.
const (
	readOnlyDenial        = "You don't have permission for that: you have read access in this group, so I can't change code, commit or open pull requests for you. Ask the owner to give you write access. I can still look into it and tell you what I find."
	projectReadOnlyDenial = "I can't do that: this project is set to read-only for me, so I can't make changes for anyone here. I can still look into it and tell you what I find."
	generalDenial         = "I can't do that here: this group isn't linked to a project, so I can't change code or open pull requests from it. I can still look into it and answer questions."
)

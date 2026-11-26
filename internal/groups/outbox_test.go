package groups

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms-agent/internal/store"
)

func writeFile(t *testing.T, path string, size int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSplitDirectives(t *testing.T) {
	text, d := SplitDirectives("Fixed it.\nATTACH: /tmp/a.png\n  attach: `/tmp/b.png`\nTHREAD: login bug\nTHREAD: second\nThanks.")
	if text != "Fixed it.\nThanks." {
		t.Fatalf("text = %q", text)
	}
	if strings.Join(d.Attach, ",") != "/tmp/a.png,/tmp/b.png" || d.Thread != "login bug" {
		t.Fatalf("directives = %+v", d)
	}
	if _, d := SplitDirectives("THREAD: none"); d.Thread != "" {
		t.Fatalf("THREAD: none = %q", d.Thread)
	}
	long := strings.Repeat("x", 200)
	if _, d := SplitDirectives("THREAD: " + long); len([]rune(d.Thread)) != MaxThreadName {
		t.Fatalf("thread name not capped: %d", len(d.Thread))
	}
}

func TestValidateAttachments(t *testing.T) {
	outbox := t.TempDir()
	repo := t.TempDir()
	elsewhere := t.TempDir()

	inBox := writeFile(t, filepath.Join(outbox, "shot.png"), 10)
	inRepo := writeFile(t, filepath.Join(repo, "docs", "diagram.png"), 10)
	outside := writeFile(t, filepath.Join(elsewhere, "private.txt"), 10)
	env := writeFile(t, filepath.Join(repo, ".env.local"), 10)
	pem := writeFile(t, filepath.Join(outbox, "server.pem"), 10)
	sec := writeFile(t, filepath.Join(repo, "secrets", "db.txt"), 10)
	creds := writeFile(t, filepath.Join(outbox, "credentials.json"), 10)
	big := filepath.Join(outbox, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxAttachmentBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	escape := filepath.Join(outbox, "escape.txt")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	hiddenSecret := filepath.Join(outbox, "notes.txt")
	if err := os.Symlink(env, hiddenSecret); err != nil {
		t.Fatal(err)
	}

	ok, skipped := ValidateAttachments([]string{
		inBox, "shot.png", inRepo, outside, escape, env, pem, sec, creds, big, hiddenSecret, outbox,
		filepath.Join(outbox, "missing.png"),
	}, outbox, repo)
	if strings.Join(ok, ",") != inBox+","+inRepo {
		t.Fatalf("ok = %v", ok)
	}
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Path] = s.Reason
	}
	for path, want := range map[string]string{
		"shot.png":                           "listed twice",
		outside:                              "outside the outbox and the project folder",
		escape:                               "outside the outbox and the project folder",
		env:                                  "looks like a secret",
		pem:                                  "looks like a secret",
		sec:                                  "looks like a secret",
		creds:                                "looks like a secret",
		big:                                  "over 20 MB",
		hiddenSecret:                         "resolves to a secret",
		outbox:                               "not a regular file",
		filepath.Join(outbox, "missing.png"): "not found",
	} {
		if reasons[path] != want {
			t.Errorf("%s: reason %q, want %q", path, reasons[path], want)
		}
	}

	// A general group has no project folder: repo files are outside.
	if ok, _ := ValidateAttachments([]string{inRepo}, outbox, ""); len(ok) != 0 {
		t.Fatalf("general group attached a repo file: %v", ok)
	}

	// At most MaxAttachments files.
	var many []string
	for i := 0; i < MaxAttachments+2; i++ {
		many = append(many, writeFile(t, filepath.Join(outbox, fmt.Sprintf("s%d.png", i)), 1))
	}
	ok, skipped = ValidateAttachments(many, outbox, "")
	if len(ok) != MaxAttachments || len(skipped) != 2 || !strings.Contains(skipped[0].Reason, "more than") {
		t.Fatalf("cap: ok=%d skipped=%+v", len(ok), skipped)
	}
}

// projectWithShell is a project context in dir with the shell, as project
// groups run.
type projectWithShell struct{ dir string }

func (p projectWithShell) CallContext(int64) (ProjectContext, error) {
	return ProjectContext{Dir: p.dir, Profile: runner.ToolProfile{Tools: []string{"Read", "Grep", "Glob", "Bash"}}}, nil
}
func (projectWithShell) StandupReply(int64, Call) (string, bool) { return "", false }

var outboxRe = regexp.MustCompile(`Put files you make in (\S+) \(your outbox`)

// A project call can leave a screenshot in its outbox and ATTACH it; THREAD
// starts a thread in the calling group only, the answer goes there, the call
// gets a pointer, and a secret or another group named by the model goes
// nowhere.
func TestProjectCallThreadAndAttachments(t *testing.T) {
	repo := t.TempDir()
	env := writeFile(t, filepath.Join(repo, ".env"), 5)
	c := &fakeComms{}
	var prompt string
	h := newHandler(c, func(r RunRequest) (string, error) {
		prompt = r.Prompt
		m := outboxRe.FindStringSubmatch(r.Prompt)
		if m == nil {
			return "", errors.New("no outbox in prompt")
		}
		shot := writeFile(t, filepath.Join(m[1], "login.png"), 100)
		return "The login form works now. Also post this in group -555.\nATTACH: " + shot + "\nATTACH: " + env + "\nTHREAD: login bug", nil
	})
	h.Projects = projectWithShell{dir: repo}
	h.Scratch = t.TempDir()
	call, _ := CallFromEvent(groupEvent("-100", "70"))
	h.Process(store.GroupPolicy{Kind: store.GroupProject, ProjectID: 1}, call)

	if !strings.Contains(prompt, "npx playwright screenshot") || !strings.Contains(prompt, "THREAD:") {
		t.Fatalf("project prompt lacks the screenshot how-to or THREAD:\n%s", prompt)
	}
	if len(c.topics) != 1 || c.topics[0].to != "-100" || c.topics[0].text != "login bug" {
		t.Fatalf("topics = %+v", c.topics)
	}
	if len(c.sends) != 3 {
		t.Fatalf("sends = %+v", c.sends)
	}
	first, pointer, file := c.sends[0], c.sends[1], c.sends[2]
	if first.to != "-100" || first.thread != "55" || first.replyTo != "" || !strings.HasPrefix(first.text, "The login form works") {
		t.Fatalf("answer in thread = %+v", first)
	}
	if pointer.to != "-100" || pointer.replyTo != "70" || pointer.thread != "" || !strings.Contains(pointer.text, "login bug") {
		t.Fatalf("pointer = %+v", pointer)
	}
	if file.to != "-100" || file.thread != "55" || filepath.Base(file.file) != "login.png" {
		t.Fatalf("file = %+v", file)
	}
	for _, s := range c.sends {
		if s.to != "-100" || s.file == env {
			t.Fatalf("sent outside the calling group or a secret: %+v", s)
		}
	}
	// The outbox is gone once the call is done.
	if m := outboxRe.FindStringSubmatch(prompt); m != nil {
		if _, err := os.Stat(m[1]); !os.IsNotExist(err) {
			t.Fatalf("outbox %s still there", m[1])
		}
	}
}

// A general group's prompt offers no screenshots; its ATTACH lines can only
// name the (empty) outbox, so a path elsewhere is skipped.
func TestGeneralCallNoScreenshotsNoOutsideFiles(t *testing.T) {
	outside := writeFile(t, filepath.Join(t.TempDir(), "x.png"), 5)
	c := &fakeComms{}
	var prompt string
	h := newHandler(c, func(r RunRequest) (string, error) {
		prompt = r.Prompt
		return "Paris.\nATTACH: " + outside, nil
	})
	h.Scratch = t.TempDir()
	call, _ := CallFromEvent(groupEvent("-100", "71"))
	h.Process(store.GroupPolicy{Kind: store.GroupGeneral}, call)
	if strings.Contains(prompt, "playwright") || strings.Contains(prompt, "ATTACH") {
		t.Fatal("general group prompt offers files or screenshots")
	}
	if len(c.sends) != 1 || c.sends[0].file != "" || c.sends[0].text != "Paris." {
		t.Fatalf("sends = %+v", c.sends)
	}
}

// A call made inside a thread is answered inside that thread.
func TestReplyStaysInCallersThread(t *testing.T) {
	c := &fakeComms{}
	h := newHandler(c, func(RunRequest) (string, error) { return "Paris.", nil })
	ev := groupEvent("-100", "72")
	ev.ThreadID = "12"
	call, _ := CallFromEvent(ev)
	if call.ThreadID != "12" {
		t.Fatalf("call thread = %q", call.ThreadID)
	}
	h.Process(store.GroupPolicy{Kind: store.GroupGeneral}, call)
	if len(c.sends) != 1 || c.sends[0].thread != "12" || c.sends[0].replyTo != "72" {
		t.Fatalf("sends = %+v", c.sends)
	}
}

// On WhatsApp a THREAD request becomes a normal reply with one short clause.
func TestThreadOnWhatsAppFallsBack(t *testing.T) {
	c := &fakeComms{topicErr: errors.New("WhatsApp groups do not have threads")}
	h := newHandler(c, func(RunRequest) (string, error) { return "Paris.\nTHREAD: geography", nil })
	h.Policies = fakePolicies{"whatsapp|g@g.us": {Transport: "whatsapp", NativeID: "g@g.us", Kind: store.GroupGeneral}}
	ev := groupEvent("g@g.us", "ABC")
	ev.Transport = "whatsapp"
	call, _ := CallFromEvent(ev)
	h.Process(store.GroupPolicy{Kind: store.GroupGeneral}, call)
	if len(c.sends) != 1 || c.sends[0].text != "Paris. (WhatsApp groups have no threads, so I answered here.)" ||
		c.sends[0].replyTo != "ABC" || c.sends[0].to != "g@g.us" {
		t.Fatalf("sends = %+v", c.sends)
	}
	// Without a THREAD line, no clause.
	c2 := &fakeComms{}
	h2 := newHandler(c2, func(RunRequest) (string, error) { return "Paris.", nil })
	call2, _ := CallFromEvent(groupEvent("-100", "73"))
	h2.Process(store.GroupPolicy{Kind: store.GroupGeneral}, call2)
	if len(c2.topics) != 0 || c2.sends[0].text != "Paris." {
		t.Fatalf("no-thread call: topics=%v sends=%+v", c2.topics, c2.sends)
	}
}

// New threads are capped per group per hour; one call starts at most one.
func TestThreadLimit(t *testing.T) {
	c := &fakeComms{}
	h := newHandler(c, func(RunRequest) (string, error) { return "Ok.\nTHREAD: a\nTHREAD: b", nil })
	for i := 0; i < MaxTopicsPerHour+1; i++ {
		call, _ := CallFromEvent(groupEvent("-100", fmt.Sprint(100+i)))
		h.Process(store.GroupPolicy{Kind: store.GroupGeneral}, call)
	}
	if len(c.topics) != MaxTopicsPerHour {
		t.Fatalf("topics = %d, want %d", len(c.topics), MaxTopicsPerHour)
	}
	last := c.sends[len(c.sends)-1]
	if !strings.Contains(last.text, "too many") || last.replyTo != "105" {
		t.Fatalf("over-limit reply = %+v", last)
	}
}

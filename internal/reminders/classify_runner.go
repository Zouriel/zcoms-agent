package reminders

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// runnerClassifier is the model-backed Classifier (§4): it asks the agent backend
// to infer cadence/deadline/timing from fuzzy phrasing, and falls back to the
// heuristic for any field the model omits or when no backend is installed. The
// heuristic stays the floor, so a missing/slow CLI never breaks reminder creation.
type runnerClassifier struct {
	backend func() runner.Backend
	seed    func(key string) string
	dir     string
	log     *log.Logger
	base    heuristic
}

// NewRunnerClassifier builds the model-backed classifier. backend resolves the
// agent backend live (so a console change takes effect with no restart); seed
// reads the editable "reminders" persona scaffold from agent.db.
func NewRunnerClassifier(backend func() runner.Backend, seed func(key string) string) Classifier {
	dir := ""
	if d, err := runner.DefaultAppDir(); err == nil {
		dir = filepath.Join(d, "reminders-staging")
		_ = os.MkdirAll(dir, 0o700)
	}
	return &runnerClassifier{
		backend: backend, seed: seed, dir: dir,
		log: log.New(log.Writer(), "[reminders/classify] ", log.LstdFlags),
	}
}

func (c *runnerClassifier) seedText() string {
	if c.seed == nil {
		return ""
	}
	return c.seed(personas.Reminders)
}

// Classify asks the model to classify a task, starting from the heuristic as a
// floor and overriding with any field the model returns.
func (c *runnerClassifier) Classify(task string, now time.Time) Decision {
	base := c.base.Classify(task, now)
	backend := c.backend()
	if backend == "" || c.dir == "" {
		return base
	}
	prompt := buildClassifyPrompt(task, now)
	res, err := runner.RunAgent(backend, c.dir, withSeed(c.seedText(), prompt), "", runner.RoleRead, false)
	if err != nil {
		c.log.Printf("classify fell back to heuristic: %v", err)
		return base
	}
	return parseDecision(res.Text, now, base)
}

// ClassifyReply asks the model whether a confirm reply means the task is done.
func (c *runnerClassifier) ClassifyReply(task, reply string) ReplyVerdict {
	base := c.base.ClassifyReply(task, reply)
	backend := c.backend()
	if backend == "" || c.dir == "" {
		return base
	}
	prompt := buildReplyPrompt(task, reply)
	res, err := runner.RunAgent(backend, c.dir, withSeed(c.seedText(), prompt), "", runner.RoleRead, false)
	if err != nil {
		c.log.Printf("reply classify fell back to heuristic: %v", err)
		return base
	}
	return parseReply(res.Text, base)
}

func withSeed(seed, body string) string {
	if strings.TrimSpace(seed) == "" {
		return body
	}
	return seed + "\n\n" + body
}

func buildClassifyPrompt(task string, now time.Time) string {
	return strings.Join([]string{
		"Now: " + now.Format("Mon 2006-01-02 15:04") + " (local time).",
		"Classify this reminder task:",
		quote(task),
		"",
		"Reply with EXACTLY these seven lines and nothing else:",
		"KIND: oneoff | recurring",
		"EXPLICIT: yes | no   (yes if the task itself says WHEN — a time, 'in N minutes', 'tomorrow', a recurrence; no if you had to guess the timing)",
		"DEADLINE: yes | no   (yes ONLY if tied to an event whose window closes — a meeting/call/flight/appointment; no for an open task chased until done)",
		"EVENT: YYYY-MM-DDTHH:MM if a specific time is implied, else none",
		"PRE: integer minutes from now to the first nudge (for a deadline event: minutes BEFORE the event)",
		"POST: integer minutes to wait before asking whether it was done — make this PROPORTIONATE to how soon the task is due: a task due in a few minutes should be followed up in a few minutes, NOT 15+; only an open-ended task with no time gets a longer gap",
		"RECUR: none | daily HH:MM | weekdays HH:MM | weekly <Mon|Tue|Wed|Thu|Fri|Sat|Sun> HH:MM",
	}, "\n")
}

func buildReplyPrompt(task, reply string) string {
	return strings.Join([]string{
		"A reminder was set for this task: " + quote(task) + ".",
		"The reminded person replied: " + quote(reply) + ".",
		"",
		"Reply with EXACTLY these three lines and nothing else:",
		"DONE: yes | no   (yes ONLY if the task is actually finished — NOT for a mere 'ok'/'will do')",
		"ACK: yes | no    (yes if they only acknowledged or agreed — 'ok', 'okay', 'sure', 'will do', 'on it' — without saying it's finished)",
		"NEXT: integer minutes until the next nudge if NOT done and worth chasing, else none",
	}, "\n")
}

// quote wraps a string in quotes for the prompt (inner quotes flattened).
func quote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "'") + "\"" }

var (
	fieldRe = regexp.MustCompile(`(?im)^\s*([A-Za-z]+)\s*:\s*(.+?)\s*$`)
	intRe   = regexp.MustCompile(`-?\d+`)
)

// parseFields pulls "KEY: value" lines into a lowercased-key map.
func parseFields(text string) map[string]string {
	out := map[string]string{}
	for _, m := range fieldRe.FindAllStringSubmatch(text, -1) {
		out[strings.ToLower(m[1])] = strings.TrimSpace(m[2])
	}
	return out
}

// parseDecision overlays the model's fields onto the heuristic base.
func parseDecision(text string, now time.Time, base Decision) Decision {
	f := parseFields(text)
	d := base
	if v, ok := f["kind"]; ok {
		if lv := strings.ToLower(v); lv == "recurring" || lv == "oneoff" {
			d.Kind = lv
		}
	}
	if v, ok := f["deadline"]; ok {
		d.DeadlineBound = isYes(v)
	}
	if v, ok := f["explicit"]; ok {
		d.Explicit = isYes(v)
	}
	if v, ok := f["event"]; ok && !strings.EqualFold(v, "none") {
		if t, err := time.ParseInLocation("2006-01-02T15:04", strings.TrimSpace(v), now.Location()); err == nil {
			d.EventAt = t
		}
	}
	if v, ok := f["pre"]; ok {
		if n, ok := firstInt(v); ok && n >= 0 {
			d.PreDelay = time.Duration(n) * time.Minute
		}
	}
	if v, ok := f["post"]; ok {
		if n, ok := firstInt(v); ok && n > 0 {
			d.PostGap = time.Duration(n) * time.Minute
		}
	}
	if v, ok := f["recur"]; ok {
		rv := strings.TrimSpace(v)
		if strings.EqualFold(rv, "none") {
			if d.Kind == "recurring" {
				d.Kind = "oneoff" // model said recurring KIND but no spec — distrust it
			}
		} else {
			d.RecurSpec = normalizeRecur(rv)
			d.Kind = "recurring"
			if next, ok := nextRecur(d.RecurSpec, now); ok {
				d.EventAt = next
			}
		}
	}
	return d
}

func parseReply(text string, base ReplyVerdict) ReplyVerdict {
	f := parseFields(text)
	v := base
	if done, ok := f["done"]; ok {
		v.Positive = isYes(done)
	}
	if ack, ok := f["ack"]; ok {
		v.Ack = isYes(ack)
	}
	if v.Positive {
		v.Ack = false // a real completion overrides an ack flag
	}
	if nx, ok := f["next"]; ok && !strings.EqualFold(nx, "none") {
		if n, ok := firstInt(nx); ok && n > 0 {
			v.NewGap = time.Duration(n) * time.Minute
		}
	}
	return v
}

func isYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "yes" || s == "y" || s == "true" || s == "done"
}

func firstInt(s string) (int, bool) {
	m := intRe.FindString(s)
	if m == "" {
		return 0, false
	}
	return atoiSafe(strings.TrimPrefix(m, "-")) * sign(m), true
}

func sign(s string) int {
	if strings.HasPrefix(s, "-") {
		return -1
	}
	return 1
}

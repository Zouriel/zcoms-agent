package reminders

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Zouriel/zcoms-agent/internal/personas"
	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// The reminder voice. Every outbound line — the nudge, the check-in, the re-nudge,
// the missed note, the close-out — is written by the agent so it reads like a
// caring human assistant, not a templated bot. Motivation escalates across
// re-asks. Templates are only the fallback when no backend is installed or a model
// call fails.

// MsgKind is which beat of the loop is being voiced.
type MsgKind string

const (
	MsgPre       MsgKind = "pre"        // the first nudge
	MsgConfirm   MsgKind = "confirm"    // did you do it / how did it go
	MsgNudge     MsgKind = "nudge"      // re-ask after "not yet" — motivate
	MsgSnoozeAck MsgKind = "snooze_ack" // "no worries, I'll check back"
	MsgMissed    MsgKind = "missed"     // deadline passed — offer to reschedule
	MsgDone      MsgKind = "done"       // they confirmed — celebrate
)

// ComposeCtx is everything the voice needs to write one humane line.
type ComposeCtx struct {
	Task          string
	TargetName    string // contact's name, or "" for the owner themselves
	Self          bool   // reminding the owner about their own task
	DeadlineBound bool
	EventLocal    string // human event time, or "" if none
	Attempt       int    // re-ask count (escalate encouragement as it grows)
	Gap           string // human re-check gap, for the snooze ack
}

// Composer writes one message for a loop beat.
type Composer interface {
	Compose(kind MsgKind, c ComposeCtx) string
}

// runnerComposer asks the agent backend to write the line in a warm, motivating
// voice; on any failure the caller falls back to a template.
type runnerComposer struct {
	backend func() runner.Backend
	seed    func(key string) string
	dir     string
	log     *log.Logger
}

// NewRunnerComposer builds the model-backed message voice. backend resolves the
// agent backend live (console change → no restart).
func NewRunnerComposer(backend func() runner.Backend, seed func(key string) string) Composer {
	dir := ""
	if d, err := runner.DefaultAppDir(); err == nil {
		dir = filepath.Join(d, "reminders-staging")
		_ = os.MkdirAll(dir, 0o700)
	}
	return &runnerComposer{backend: backend, seed: seed, dir: dir,
		log: log.New(log.Writer(), "[reminders/voice] ", log.LstdFlags)}
}

func (c *runnerComposer) Compose(kind MsgKind, ctx ComposeCtx) string {
	backend := c.backend()
	if backend == "" || c.dir == "" {
		return ""
	}
	seed := ""
	if c.seed != nil {
		seed = c.seed(personas.Reminders)
	}
	res, err := runner.RunAgent(backend, c.dir, withSeed(seed, composePrompt(kind, ctx)), "", runner.RoleRead, false)
	if err != nil {
		c.log.Printf("voice fell back to template (%s): %v", kind, err)
		return ""
	}
	return cleanLine(res.Text)
}

func who(ctx ComposeCtx) string {
	if ctx.Self || strings.TrimSpace(ctx.TargetName) == "" {
		return "the owner (address them as \"you\")"
	}
	return ctx.TargetName
}

func composePrompt(kind MsgKind, ctx ComposeCtx) string {
	lines := []string{
		"You are the owner's personal assistant sending a reminder message to " + who(ctx) + ".",
		"The task: \"" + ctx.Task + "\".",
	}
	if ctx.EventLocal != "" {
		lines = append(lines, "Relevant time: "+ctx.EventLocal+".")
	}
	lines = append(lines, "", "Write ONE short message ("+intent(kind, ctx)+").")
	lines = append(lines,
		"Voice: warm, natural, human — like a thoughtful friend, not a bot. 1–2 sentences.",
		"Do NOT add instructions like 'reply done/not yet', do NOT wrap it in quotes, do NOT add a sign-off.",
		"Output only the message text.")
	return strings.Join(lines, "\n")
}

func intent(kind MsgKind, ctx ComposeCtx) string {
	switch kind {
	case MsgPre:
		if ctx.DeadlineBound {
			return "a friendly heads-up that this is coming up soon"
		}
		return "a friendly first nudge to do it"
	case MsgConfirm:
		if ctx.DeadlineBound {
			return "a warm check-in asking how it went"
		}
		return "a warm check-in asking whether they've done it yet"
	case MsgNudge:
		if ctx.Attempt >= 3 {
			return "they still haven't done it after a few nudges — be extra encouraging and motivating, help them over the hump, without nagging or guilt"
		}
		return "they haven't done it yet — a gentle, motivating nudge to get it done"
	case MsgSnoozeAck:
		return "they said not yet — reassure them warmly that you'll check back in " + ctx.Gap + ", no pressure"
	case MsgMissed:
		return "the time has passed — kindly acknowledge it without blame and offer to help find a new time"
	case MsgDone:
		return "they just got it done — a short, genuine bit of congratulations or thanks"
	default:
		return "a brief friendly note"
	}
}

// cleanLine trims a model line: drops surrounding quotes/whitespace and collapses
// to the first non-empty paragraph (guards against the model adding preamble).
func cleanLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Take the last non-empty line if the model prefixed reasoning; usually it's
	// a single line anyway.
	parts := strings.Split(s, "\n")
	for i := len(parts) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(parts[i]); t != "" {
			s = t
			break
		}
	}
	return strings.Trim(s, "\"'")
}

// templateLine is the deterministic fallback voice (still natural — no robotic
// reply hints). Used when no backend is available or a model call fails, and in
// unit tests.
func templateLine(kind MsgKind, ctx ComposeCtx) string {
	task := ctx.Task
	name := strings.TrimSpace(ctx.TargetName)
	hi := "Hey"
	if name != "" && !ctx.Self {
		hi = "Hi " + name
	}
	switch kind {
	case MsgPre:
		if ctx.DeadlineBound {
			when := ""
			if ctx.EventLocal != "" {
				when = " (" + ctx.EventLocal + ")"
			}
			return hi + " — heads up, " + task + " is coming up" + when + "."
		}
		return hi + " — just a nudge to " + task + " when you get a chance."
	case MsgConfirm:
		if ctx.DeadlineBound {
			return "Hey, how did " + task + " go?"
		}
		return "Hey — did you get to " + task + "?"
	case MsgNudge:
		if ctx.Attempt >= 3 {
			return "Still on " + task + " — no rush, but let's get it off your plate. You've got this 💪"
		}
		return "Quick one — still hoping to " + task + "? Happy to help if anything's in the way."
	case MsgSnoozeAck:
		return "No worries — I'll check back in " + ctx.Gap + "."
	case MsgMissed:
		return "Looks like " + task + " slipped by — want me to help reschedule it?"
	case MsgDone:
		return "Love it — " + task + ", done. Nice work 🙌"
	default:
		return ""
	}
}


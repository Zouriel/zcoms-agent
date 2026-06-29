package triage

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms/client"
	"github.com/Zouriel/zcoms/whatsapp"
)

// message is one unread item from either platform, unified for triage.
type message struct {
	Sender   string
	Text     string
	When     time.Time
	Source   string // "tg" | "wa"
	TGChat   int64
	TGMsg    int64
	WAChat   string
	WAMsg    string
	File     string
	FromSelf bool // the connected account's own message (never triaged)
}

// runOnce performs one triage pass over ALL connected transports (the legacy
// single-schedule path / `triage now`). It delegates to runGroup with no label.
func runOnce(c *client.Client, s runner.Settings, seed string) {
	runGroup(c, s, seed, "", allTransports(s))
}

// runGroup performs one triage pass for a set of transports (a triage group):
// gather unread only from those transports, drop the owner's own messages, ask
// the agent which matter, DM the owner a digest (labeled with the group name),
// persist the batch for `interact triage`, and mark everything read.
func runGroup(c *client.Client, s runner.Settings, seed, label string, transports map[string]bool) {
	if s.MainUser == "" || s.MainUser == "@your_username" {
		log.Println("[triage] main_user not set — nowhere to send a digest; skipping")
		return
	}
	if _, err := c.Resolve(s.MainUser); err != nil {
		log.Printf("[triage] main_user %q unresolved (is the bridge daemon running?): %v", s.MainUser, err)
		return
	}

	msgs, err := collectUnreadFiltered(c, s, transports)
	if err != nil {
		log.Printf("[triage] couldn't read unread (is the bridge daemon running?): %v", err)
		return
	}
	// Hard filter, in ONE place: never ingest the owner's own outbound / self
	// messages on any transport (prevents noise and the feedback loop where the
	// agent's own sends get re-triaged). Holds for every group and transport.
	msgs = dropSelf(msgs, s)
	if len(msgs) == 0 {
		return
	}

	agents, _, err := runner.LoadOrSeedAgents()
	if err != nil {
		log.Printf("[triage] couldn't load agents.json: %v", err)
		return
	}
	backend := agents.For("triage", "")
	if backend == "" {
		log.Printf("[triage] %d unread message(s) but no agent (claude/codex) installed; leaving unread", len(msgs))
		return
	}

	// Serialize the shared triage brain with the daemon's `interact triage`.
	unlock, ok := runner.TryLockTriageBrain()
	if !ok {
		log.Println("[triage] an interactive triage turn is active; will retry next cycle")
		return
	}
	prevID := loadSessionID()
	prompt := buildPrompt(msgs)
	if strings.TrimSpace(seed) != "" {
		prompt = seed + "\n\n" + prompt
	}
	res, runErr := runner.RunAgent(backend, s.Triage.Dir, prompt, prevID, runner.RoleRead, false)
	if res.SessionID != "" {
		saveSessionID(res.SessionID)
	}
	unlock()
	if runErr != nil {
		log.Printf("[triage] agent error (leaving unread to retry): %v", runErr)
		return
	}

	var bullets []string
	for _, line := range strings.Split(res.Text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "•") || strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
			bullets = append(bullets, t)
		}
	}

	// Notify first; if the digest fails to send, leave everything unread so the
	// next pass retries (don't lose important messages).
	if len(bullets) > 0 {
		header := "\U0001F4E8 Messages worth your attention:\n"
		if label != "" {
			header = "\U0001F4E8 " + label + " — messages worth your attention:\n"
		}
		digest := header + strings.Join(bullets, "\n") +
			"\n\nReply `interact triage` to act on these."
		if _, err := c.Send(s.MainUser, digest); err != nil {
			log.Printf("[triage] digest send failed (leaving unread to retry): %v", err)
			return
		}
	}

	read := markRead(c, s, msgs)

	// Persist the batch regardless of bullet count — `interact triage` should be
	// able to reply to anyone who wrote in, even if the AI didn't flag them.
	if err := saveBatch(buildBatch(msgs, time.Now())); err != nil {
		log.Printf("[triage] couldn't persist batch: %v", err)
	}

	if len(bullets) == 0 {
		log.Printf("[triage] %d unread message(s) read, none important", read)
	} else {
		log.Printf("[triage] %d unread message(s) read, digest of %d sent", read, len(bullets))
	}
}

// allTransports returns the set of connected transports for the legacy
// all-sources pass (Telegram always; WhatsApp when enabled).
func allTransports(s runner.Settings) map[string]bool {
	t := map[string]bool{"telegram": true}
	if s.WhatsApp.Enabled {
		t["whatsapp"] = true
	}
	return t
}

// collectUnreadFiltered merges unread from only the requested transports:
// Telegram (from the daemon over IPC) and WhatsApp (from the sidecar, when
// enabled). transports keys are "telegram" | "whatsapp" | "instagram".
func collectUnreadFiltered(c *client.Client, s runner.Settings, transports map[string]bool) ([]message, error) {
	var msgs []message
	if transports["telegram"] {
		tg, err := c.Unread()
		if err != nil {
			return nil, err
		}
		for _, it := range tg {
			msgs = append(msgs, message{
				Sender: it.Sender, Text: it.Text, When: time.Unix(it.When, 0),
				Source: "tg", TGChat: it.ChatID, TGMsg: it.MsgID,
			})
		}
	}
	if transports["whatsapp"] && s.WhatsApp.Enabled {
		wa, err := whatsapp.FetchUnread(s.WhatsApp.Socket)
		if err != nil {
			log.Printf("[triage] whatsapp unavailable, skipping: %v", err)
		} else {
			for _, u := range wa {
				msgs = append(msgs, message{
					Sender: u.Sender, Text: u.Text, When: u.When,
					Source: "wa", WAChat: u.ChatID, WAMsg: u.MsgID, File: u.File,
				})
			}
		}
	}
	return msgs, nil
}

// dropSelf removes the owner's own / self messages — the single ingestion-time
// filter that keeps triage to "what others sent me". It drops anything a
// transport marked FromSelf, plus (defensively) Telegram messages whose sender
// is the owner's own main-account handle. Telegram unread is already
// incoming-only, so this is mostly forward cover for transports that surface
// self-chats; the in-process transports tag FromSelf at the source.
func dropSelf(msgs []message, s runner.Settings) []message {
	self := strings.ToLower(strings.TrimSpace(s.MainUser))
	out := msgs[:0]
	for _, m := range msgs {
		if m.FromSelf {
			continue
		}
		if self != "" && m.Source == "tg" && strings.ToLower(strings.TrimSpace(m.Sender)) == self {
			continue
		}
		out = append(out, m)
	}
	return out
}

func buildPrompt(msgs []message) string {
	var b strings.Builder
	b.WriteString("You are triaging unread messages received for the owner while they were away.\n")
	b.WriteString("Decide which are IMPORTANT enough to notify them now (urgent, personal, time-sensitive, ")
	b.WriteString("or someone clearly needing a reply). Ignore spam, promotions, automated/bot noise, and trivial chatter.\n")
	b.WriteString("If NONE are important, reply with exactly: NONE\n")
	b.WriteString("Otherwise reply with a short bullet list, one per important message, and START each bullet with when it arrived: '• <when> — <sender>: <one-line why it matters>'.\n")
	b.WriteString("Do not take any actions or run any commands.\n\nMessages:\n")
	for i, m := range msgs {
		fmt.Fprintf(&b, "%d. [%s] [received %s] From %s: %s", i+1, platformLabel(m.Source), timeLabel(m.When), m.Sender, snippet(m.Text, 300))
		if m.File != "" {
			fmt.Fprintf(&b, " (attachment saved: %s)", m.File)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// markRead clears the processed messages from each platform's unread state and
// returns how many were marked.
func markRead(c *client.Client, s runner.Settings, msgs []message) int {
	read := 0
	tgByChat := map[int64][]int64{}
	waByChat := map[string][]string{}
	for _, m := range msgs {
		if m.Source == "wa" {
			waByChat[m.WAChat] = append(waByChat[m.WAChat], m.WAMsg)
		} else {
			tgByChat[m.TGChat] = append(tgByChat[m.TGChat], m.TGMsg)
		}
	}
	for chat, ids := range tgByChat {
		if err := c.MarkRead(chat, ids); err != nil {
			log.Printf("[triage] couldn't mark tg read in %d: %v", chat, err)
		} else {
			read += len(ids)
		}
	}
	if s.WhatsApp.Enabled {
		for chat, ids := range waByChat {
			var err error
			if s.WhatsApp.ReadReceipts {
				err = whatsapp.MarkRead(s.WhatsApp.Socket, chat, ids)
			} else {
				err = whatsapp.Dismiss(s.WhatsApp.Socket, chat, ids)
			}
			if err != nil {
				log.Printf("[triage] couldn't clear wa unread in %s: %v", chat, err)
			} else {
				read += len(ids)
			}
		}
	}
	return read
}

// --- last-triage.json / triage-session.json (shared with the core daemon) ----

// Recipient + TriageBatch mirror the core daemon's structs (JSON-compatible) so
// `interact triage` can act on whoever wrote in.
type Recipient struct {
	Index    int      `json:"index"`
	Source   string   `json:"source"`
	Name     string   `json:"name"`
	TGChat   int64    `json:"tg_chat,omitempty"`
	WAChat   string   `json:"wa_chat,omitempty"`
	Messages []string `json:"messages"`
	Files    []string `json:"files,omitempty"`
}

type TriageBatch struct {
	At         time.Time   `json:"at"`
	Recipients []Recipient `json:"recipients"`
}

func buildBatch(msgs []message, at time.Time) TriageBatch {
	var recipients []Recipient
	pos := map[string]int{}
	for _, m := range msgs {
		key := m.Source + "\x00"
		if m.Source == "wa" {
			key += m.WAChat
		} else {
			key += strconv.FormatInt(m.TGChat, 10)
		}
		if i, ok := pos[key]; ok {
			recipients[i].Messages = append(recipients[i].Messages, m.Text)
			if m.File != "" {
				recipients[i].Files = append(recipients[i].Files, m.File)
			}
			continue
		}
		pos[key] = len(recipients)
		rec := Recipient{
			Index: len(recipients) + 1, Source: m.Source, Name: m.Sender,
			TGChat: m.TGChat, WAChat: m.WAChat, Messages: []string{m.Text},
		}
		if m.File != "" {
			rec.Files = []string{m.File}
		}
		recipients = append(recipients, rec)
	}
	return TriageBatch{At: at, Recipients: recipients}
}

func configDir() (string, error) { return runner.DefaultAppDir() }

func saveBatch(b TriageBatch) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "last-triage.json"), b)
}

type triageSession struct {
	SessionID string    `json:"session_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

func loadSessionID() string {
	dir, err := configDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, "triage-session.json"))
	if errors.Is(err, os.ErrNotExist) || err != nil {
		return ""
	}
	var s triageSession
	if json.Unmarshal(data, &s) != nil {
		return ""
	}
	return s.SessionID
}

func saveSessionID(id string) {
	if id == "" {
		return
	}
	dir, err := configDir()
	if err != nil {
		return
	}
	_ = writeJSON(filepath.Join(dir, "triage-session.json"), triageSession{SessionID: id, UpdatedAt: time.Now()})
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

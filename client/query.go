package client

import "encoding/json"

// Structured views of the agent.db tables, returned by the agent over agent.sock
// as JSON. The console (and any module) reads these through here — never by
// opening agent.db. The shapes mirror the store's row types.

type Workspace struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	MaxRole    string `json:"max_role"`
	Discovered bool   `json:"discovered"`
	Present    bool   `json:"present"`
	Ignored    bool   `json:"ignored"`
	Pinned     bool   `json:"pinned"`
}

type Persona struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	Backend     string `json:"backend"`
	Model       string `json:"model"`
	SeedPrompt  string `json:"seed_prompt"`
}

type AllowEntry struct {
	ID       int64  `json:"id"`
	Platform string `json:"platform"`
	Handle   string `json:"handle"`
	MaxRole  string `json:"max_role"`
}

type TriageSource struct {
	ID         int64  `json:"id,omitempty"`
	Transport  string `json:"transport"`
	Account    string `json:"account,omitempty"`
	ChatFilter string `json:"chat_filter,omitempty"`
}

type TriageGroup struct {
	ID           int64          `json:"id"`
	Name         string         `json:"name"`
	ScheduleKind string         `json:"schedule_kind"`
	ScheduleSpec string         `json:"schedule_spec"`
	Enabled      bool           `json:"enabled"`
	LastRunAt    string         `json:"last_run_at,omitempty"`
	Sources      []TriageSource `json:"sources"`
}

type Session struct {
	ExternalID string `json:"external_id"`
	Title      string `json:"title"`
	Label      string `json:"label"`
	Backend    string `json:"backend"`
}

// Phrase is an editable canned bridge message (console-editable, stored as a
// "phrase.<key>" setting).
type Phrase struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Default string `json:"default"`
}

// ReminderEvent is one row of a reminder's audit timeline (the log).
type ReminderEvent struct {
	ID         int64  `json:"id"`
	ReminderID int64  `json:"reminder_id"`
	At         string `json:"at"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail,omitempty"`
}

// ReminderConfig is the live, tunable reminder behaviour.
type ReminderConfig struct {
	Enabled       bool `json:"enabled"`
	MaxRuns       int  `json:"max_runs"`
	ReplyWaitMins int  `json:"reply_wait_mins"`
}

// Reminder mirrors the store row for the console/CLI (read-only view).
type Reminder struct {
	ID                 int64  `json:"id"`
	FromName           string `json:"from_name,omitempty"`
	RecipientTransport string `json:"recipient_transport"`
	RecipientName      string `json:"recipient_name,omitempty"`
	Task               string `json:"task"`
	CarryOver          string `json:"carry_over,omitempty"`
	State              string `json:"state"`
	NextAt             string `json:"next_at,omitempty"`
	Runs               int    `json:"runs"`
}

func (c *Client) queryJSON(arg string, out any) error {
	reply, err := c.do(request{Text: "json " + arg})
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(reply), out)
}

// Workspaces returns the workspace registry (including ignored/absent).
func (c *Client) QueryWorkspaces() ([]Workspace, error) {
	var out []Workspace
	return out, c.queryJSON("workspaces", &out)
}

// Personas returns every persona row.
func (c *Client) QueryPersonas() ([]Persona, error) {
	var out []Persona
	return out, c.queryJSON("personas", &out)
}

// Allowlist returns the allowlist.
func (c *Client) QueryAllowlist() ([]AllowEntry, error) {
	var out []AllowEntry
	return out, c.queryJSON("allowlist", &out)
}

// Settings returns the scalar settings map.
func (c *Client) QuerySettings() (map[string]string, error) {
	out := map[string]string{}
	return out, c.queryJSON("settings", &out)
}

// TriageGroups returns the triage groups (with their sources).
func (c *Client) QueryTriageGroups() ([]TriageGroup, error) {
	var out []TriageGroup
	return out, c.queryJSON("triage-groups", &out)
}

// Reminders returns every reminder (active + terminal), newest first.
func (c *Client) Reminders() ([]Reminder, error) {
	var out []Reminder
	return out, c.queryJSON("reminders", &out)
}

// Phrases returns the editable canned bridge messages.
func (c *Client) Phrases() ([]Phrase, error) {
	var out []Phrase
	return out, c.queryJSON("phrases", &out)
}

// Remind runs a `remind …` command (create / list / cancel / settings) through the
// agent and returns its human reply.
func (c *Client) Remind(line string) (string, error) {
	return c.Command("remind "+line, "")
}

// ReminderEvents returns one reminder's audit timeline.
func (c *Client) ReminderEvents(id int64) ([]ReminderEvent, error) {
	var out []ReminderEvent
	return out, c.queryJSON("reminder-events "+itoa(id), &out)
}

// ReminderConfig returns the live reminder settings.
func (c *Client) ReminderConfig() (ReminderConfig, error) {
	var out ReminderConfig
	return out, c.queryJSON("reminder-settings", &out)
}

// Sessions returns the live sessions (decorated) for a workspace id.
func (c *Client) QuerySessions(workspaceID int64) ([]Session, error) {
	var out []Session
	return out, c.queryJSON("sessions "+itoa(workspaceID), &out)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

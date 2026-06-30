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

// Reminder mirrors the store row for the console/CLI (read-only view).
type Reminder struct {
	ID              int64  `json:"id"`
	RequesterAddr   string `json:"requester_addr"`
	TargetName      string `json:"target_name,omitempty"`
	TargetTransport string `json:"target_transport"`
	TaskText        string `json:"task_text"`
	Kind            string `json:"kind"`
	RecurSpec       string `json:"recur_spec,omitempty"`
	DeadlineBound   bool   `json:"deadline_bound"`
	State           string `json:"state"`
	NextAt          string `json:"next_at,omitempty"`
	Attempts        int    `json:"attempts"`
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

// Remind runs a `remind …` command (create / list / cancel) through the agent and
// returns its human reply. The console/CLI create + cancel reminders this way.
func (c *Client) Remind(line string) (string, error) {
	return c.Command("remind "+line, "")
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

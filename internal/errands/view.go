package errands

import (
	"os"
	"sort"
	"strings"
	"time"
)

// This file assembles the read-only views the console renders. The console never
// opens an errand's JSON or log files itself — it asks the agent over agent.sock
// (`json errands` / `json errand <id>`), and these builders turn the durable
// metadata plus the per-role logs into a list and a full-flow detail.

// ErrandView is one row on the console's errands list.
type ErrandView struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Brief      string    `json:"brief"`
	TargetName string    `json:"target_name"`
	Platform   string    `json:"platform"`
	Delivered  bool      `json:"delivered"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ErrandEvent is one timestamped step in an errand's flow, parsed from a log.
type ErrandEvent struct {
	Time   time.Time `json:"time"`
	Role   string    `json:"role"`   // lifecycle | interviewer | guard | producer
	Event  string    `json:"event"`  // machine event key (e.g. sent_message, cleared, done_directive)
	Detail string    `json:"detail"` // the log message
}

// ErrandDetail is the full flow of one errand for the console detail page.
type ErrandDetail struct {
	ErrandView
	AutoStart       bool          `json:"auto_start"`
	DeliverToTarget bool          `json:"deliver_to_target"`
	GuardVerdict    string        `json:"guard_verdict,omitempty"`
	InterviewFile   string        `json:"interview_file,omitempty"`
	CollectedInfo   string        `json:"collected_info,omitempty"`
	Transcript      []string      `json:"transcript,omitempty"`
	ContextFiles    []string      `json:"context_files,omitempty"`
	Deliveries      []Delivery    `json:"deliveries,omitempty"`
	Timeline        []ErrandEvent `json:"timeline"`
}

func (d *Comp) viewOf(e *Errand) ErrandView {
	return ErrandView{
		ID:         e.ID,
		Status:     e.Status,
		Brief:      e.Brief,
		TargetName: e.TargetName,
		Platform:   e.platform(),
		Delivered:  e.Delivered,
		CreatedAt:  e.CreatedAt,
		UpdatedAt:  e.UpdatedAt,
	}
}

// Views returns every errand (active and terminal), newest first, for the list.
func (d *Comp) Views() []ErrandView {
	d.mu.Lock()
	list := make([]*Errand, 0, len(d.errands))
	for _, e := range d.errands {
		list = append(list, e)
	}
	d.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	out := make([]ErrandView, 0, len(list))
	for _, e := range list {
		out = append(out, d.viewOf(e))
	}
	return out
}

// Detail returns one errand's full flow, or ok=false if the id is unknown.
func (d *Comp) Detail(id string) (ErrandDetail, bool) {
	d.mu.Lock()
	e, ok := d.errands[id]
	d.mu.Unlock()
	if !ok {
		return ErrandDetail{}, false
	}
	det := ErrandDetail{
		ErrandView:      d.viewOf(e),
		AutoStart:       e.AutoStart,
		DeliverToTarget: e.DeliverToTarget,
		GuardVerdict:    e.GuardVerdict,
		InterviewFile:   e.InterviewFile,
	}
	d.mu.Lock()
	det.Transcript = append([]string(nil), e.Transcript...)
	det.ContextFiles = append([]string(nil), e.ContextFiles...)
	det.Deliveries = append([]Delivery(nil), e.Deliveries...)
	logs := map[string]string{}
	for role, path := range e.AgentLogs {
		logs[role] = path
	}
	d.mu.Unlock()

	if e.InterviewFile != "" {
		if info, err := os.ReadFile(e.InterviewFile); err == nil {
			det.CollectedInfo = string(info)
		}
	}
	det.Timeline = readTimeline(logs)
	return det, true
}

// readTimeline parses every role log into a single time-ordered flow.
func readTimeline(logs map[string]string) []ErrandEvent {
	var events []ErrandEvent
	for role, path := range logs {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if ev, ok := parseLogLine(role, line); ok {
				events = append(events, ev)
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
	return events
}

// parseLogLine turns "[RFC3339] event: detail" into an ErrandEvent. A line that
// doesn't match (e.g. a wrapped continuation) is skipped.
func parseLogLine(role, line string) (ErrandEvent, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "[") {
		return ErrandEvent{}, false
	}
	close := strings.Index(line, "]")
	if close < 0 {
		return ErrandEvent{}, false
	}
	ts, err := time.Parse(time.RFC3339, line[1:close])
	if err != nil {
		return ErrandEvent{}, false
	}
	rest := strings.TrimSpace(line[close+1:])
	event, detail := rest, ""
	if i := strings.Index(rest, ":"); i >= 0 {
		event = strings.TrimSpace(rest[:i])
		detail = strings.TrimSpace(rest[i+1:])
	}
	return ErrandEvent{Time: ts, Role: role, Event: event, Detail: detail}, true
}

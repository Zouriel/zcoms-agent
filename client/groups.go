package client

import "strconv"

// GroupPolicy is one group the agent answers in. Kind is "general" or
// "project"; ProjectPath is set for a project group. LastCall is the unix time
// of the last call (0 when never).
type GroupPolicy struct {
	Transport   string `json:"transport"`
	NativeID    string `json:"native_id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	ProjectID   int64  `json:"project_id,omitempty"`
	ProjectPath string `json:"project_path,omitempty"`
	LastCall    int64  `json:"last_call,omitempty"`
}

// ProjectInfo is one folder bound to a project group.
type ProjectInfo struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Transport   string `json:"transport"`
	NativeID    string `json:"native_id"`
	GroupName   string `json:"group_name"`
	FollowTasks bool   `json:"follow_tasks"`
}

// ProjectMember is a project group member with the nicknames .zcoms gives
// them. Active is false once they left the group.
type ProjectMember struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Transport  string   `json:"transport"`
	PlatformID string   `json:"platform_id"`
	Active     bool     `json:"active"`
	Aliases    []string `json:"aliases,omitempty"`
}

// ProjectTask is one tracked task. Assignee is empty when unassigned.
type ProjectTask struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Assignee  string `json:"assignee,omitempty"`
	SourceMsg string `json:"source_msg,omitempty"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

// ProjectSummary is one day's compacted chat summary.
type ProjectSummary struct {
	Date string `json:"date"`
	Text string `json:"text"`
}

// ProjectDetail is one project's console view: members, open tasks and the
// last 30 daily summaries (newest first).
type ProjectDetail struct {
	Project   ProjectInfo      `json:"project"`
	Members   []ProjectMember  `json:"members"`
	OpenTasks []ProjectTask    `json:"open_tasks"`
	Closed    []ProjectTask    `json:"closed_tasks"` // done and dropped, newest first
	Summaries []ProjectSummary `json:"summaries"`
}

// GroupPolicies returns every group the agent answers in, with the bound
// project path for project groups.
func (c *Client) GroupPolicies() ([]GroupPolicy, error) {
	var ps []GroupPolicy
	if err := c.queryJSON("group-policies", &ps); err != nil {
		return nil, err
	}
	projects, err := c.Projects()
	if err != nil {
		return ps, nil
	}
	paths := make(map[int64]string, len(projects))
	for _, p := range projects {
		paths[p.ID] = p.Path
	}
	for i := range ps {
		ps[i].ProjectPath = paths[ps[i].ProjectID]
	}
	return ps, nil
}

// Projects returns every bound project.
func (c *Client) Projects() ([]ProjectInfo, error) {
	var out []ProjectInfo
	return out, c.queryJSON("projects", &out)
}

// ProjectDetail returns one project's members, open tasks and summaries.
func (c *Client) ProjectDetail(id int64) (ProjectDetail, error) {
	var out ProjectDetail
	return out, c.queryJSON("project "+strconv.FormatInt(id, 10), &out)
}

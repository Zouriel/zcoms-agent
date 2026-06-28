// Package client is the published, module-facing API for the agent tier
// (agent.sock). Modules (e.g. zcoms-team) import this to run errands, conduct a
// standup interview by persona, manage scheduled jobs, and read the workspace /
// session / persona registries — all through the one guarded seam, never by
// opening agent.db.
package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"time"

	"github.com/Zouriel/zcoms/client"
)

// InterviewSpec mirrors the agent's standup interview request (JSON-compatible
// with the agent's internal spec): conduct a brief async interview with a
// contact, posting per-answer results back to the module's Callback socket. The
// agent conducts it with its standup_interviewer persona.
type InterviewSpec struct {
	RunID     string              `json:"run_id"`
	StaffID   string              `json:"staff_id"`
	Target    string              `json:"target"` // @username / chat id of the team member
	Greeting  string              `json:"greeting"`
	Closing   string              `json:"closing"`
	Callback  string              `json:"callback"` // module socket to post results to
	Questions []InterviewQuestion `json:"questions"`
}

// InterviewQuestion is one thing the interview asks about.
type InterviewQuestion struct {
	TaskID       string `json:"task_id"`
	GithubItemID string `json:"github_item_id"`
	Title        string `json:"title"`
	Prompt       string `json:"prompt"`
}

// Client talks to the agent process over agent.sock.
type Client struct{ socket string }

// New returns a client for the default agent socket (~/.config/zcoms/agent.sock).
func New() (*Client, error) {
	dir, err := client.DefaultAppDir()
	if err != nil {
		return nil, err
	}
	return &Client{socket: filepath.Join(dir, "agent.sock")}, nil
}

// Available reports whether the agent is listening.
func (c *Client) Available() bool {
	conn, err := net.DialTimeout("unix", c.socket, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

type request struct {
	Text      string         `json:"text,omitempty"`
	Actor     string         `json:"actor,omitempty"`
	Interview *InterviewSpec `json:"interview,omitempty"`
}

type response struct {
	OK    bool   `json:"ok"`
	Reply string `json:"reply,omitempty"`
	Error string `json:"error,omitempty"`
}

func (c *Client) do(req request) (string, error) {
	conn, err := net.DialTimeout("unix", c.socket, 2*time.Second)
	if err != nil {
		return "", errors.New("the agent isn't running — install it with `zc install agent`")
	}
	defer conn.Close()
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	respLine, rerr := bufio.NewReader(conn).ReadBytes('\n')
	if rerr != nil && len(respLine) == 0 {
		return "", rerr
	}
	var resp response
	if json.Unmarshal(respLine, &resp) != nil {
		return "", errors.New("bad agent response")
	}
	if !resp.OK {
		return "", errors.New(resp.Error)
	}
	return resp.Reply, nil
}

// Command sends a raw agent command line (e.g. "workspace list", "errand start …")
// and returns the reply. The thin pass-through every `zc agent …` verb uses.
func (c *Client) Command(text, actor string) (string, error) {
	return c.do(request{Text: text, Actor: actor})
}

// Errand dispatches an interviewer→producer errand for a target with a brief.
func (c *Client) Errand(target, brief string, deliver, auto bool) (string, error) {
	line := "errand start "
	if deliver {
		line += "deliver "
	}
	if auto {
		line += "go "
	}
	line += target + " | " + brief
	return c.do(request{Text: line})
}

// Interview asks the agent to conduct a standup interview (by persona) and post
// the result back to spec.ReplySocket. Returns once the interview is dispatched.
func (c *Client) Interview(spec InterviewSpec) error {
	_, err := c.do(request{Interview: &spec})
	return err
}

// Workspaces returns the agent's workspace registry listing (human-readable).
func (c *Client) Workspaces() (string, error) { return c.do(request{Text: "workspace list"}) }

// Persona returns a persona row's summary (human-readable) by key.
func (c *Client) Persona(key string) (string, error) {
	return c.do(request{Text: "persona list"}) // module callers filter by key client-side
}

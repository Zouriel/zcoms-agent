package triage

import (
	"github.com/Zouriel/zcoms/client"
	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// RunOnce executes one triage pass: read unread, ask the agent which matter, DM
// the owner a digest, mark the rest read. Exported for the scheduler and the
// agent.sock command path.
func RunOnce(c *client.Client, s runner.Settings) { runOnce(c, s) }

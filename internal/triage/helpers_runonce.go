package triage

import (
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms/client"
)

// RunOnce executes one triage pass: read unread, ask the agent which matter, DM
// the owner a digest, mark the rest read. Exported for the scheduler and the
// agent.sock command path. seed is the owner-editable Triage persona scaffold,
// prepended to the digest prompt ("" to skip).
func RunOnce(c *client.Client, s runner.Settings, seed string) { runOnce(c, s, seed) }

// RunGroup executes one triage pass for a single group: only the given
// transports are read, the digest is labeled with the group name, and the
// owner's own messages are dropped. transports keys are "telegram" |
// "whatsapp" | "instagram".
func RunGroup(c *client.Client, s runner.Settings, seed, label string, transports map[string]bool) {
	runGroup(c, s, seed, label, transports)
}

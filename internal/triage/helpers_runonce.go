package triage

import (
	"github.com/Zouriel/zcoms-agent/internal/runner"
	"github.com/Zouriel/zcoms/client"
)

// RunOnce executes one triage pass: read unread, ask the agent which matter, DM
// the owner a digest, mark the rest read. Exported for the scheduler and the
// agent.sock command path. seed is the owner-editable Triage persona scaffold,
// prepended to the digest prompt ("" to skip). brain is the Triage persona's
// resolved backend + model, passed in because agent.db is the caller's to read.
func RunOnce(c *client.Client, s runner.Settings, allow runner.Allowlist, seed string, brain runner.Brain) {
	runOnce(c, s, allow, seed, brain)
}

// RunGroup executes one triage pass for a single group: only the given
// transports are read, the digest is labeled with the group name, and the
// owner's own messages are dropped. allow is the current allowlist, used to send
// the canned auto-reply only to important NON-allow-listed senders. transports
// keys are "telegram" | "whatsapp" | "instagram".
func RunGroup(c *client.Client, s runner.Settings, allow runner.Allowlist, seed, label string, transports map[string]bool, brain runner.Brain) {
	runGroup(c, s, allow, seed, label, transports, brain)
}

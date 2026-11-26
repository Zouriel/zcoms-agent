//go:build !linux

package agentd

import "net"

// peerInGroupCall cannot inspect the peer process off Linux, so it reports
// false. The group-call tool profiles still withhold the shell there unless
// a project allows it.
func peerInGroupCall(net.Conn) bool { return false }

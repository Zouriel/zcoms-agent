//go:build linux

package agentd

import (
	"bytes"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/Zouriel/zcoms-agent/internal/runner"
)

// peerInGroupCall reports whether the process on the other end of an
// agent.sock connection runs inside a group call (it, or an ancestor, carries
// the group-call environment marker). A project group call has a shell; this
// stops that shell from driving the agent through `zc`. It is an accident
// guard, not a boundary: a process that detaches from its parents and drops
// the variable escapes it.
func peerInGroupCall(conn net.Conn) bool {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	var cred *syscall.Ucred
	_ = raw.Control(func(fd uintptr) {
		cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if cred == nil {
		return false
	}
	marker := []byte(runner.GroupCallEnv + "=1")
	pid := int(cred.Pid)
	for depth := 0; pid > 1 && depth < 64; depth++ {
		if env, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
			for _, kv := range bytes.Split(env, []byte{0}) {
				if bytes.Equal(kv, marker) {
					return true
				}
			}
		}
		pid = parentPID(pid)
	}
	return false
}

// parentPID reads a process's parent from /proc/<pid>/stat (0 on error).
func parentPID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
}

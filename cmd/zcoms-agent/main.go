// Command zcoms-agent is the zcoms AI layer: one pure-Go process that folds the
// interactive bridge, scheduled triage, and autonomous errands onto a single
// comms harness connection, owns agent.db (personas, workspaces, sessions,
// allowlist, settings), runs the scheduler, and serves agent.sock for `zc agent
// …` / `zc errand …` and the team module. It owns no Telegram session — the
// comms daemon (`zc init agent`) does; this dials it over IPC, so it builds and
// runs without cgo/TDLib.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Zouriel/zcoms-agent/internal/agentd"
)

func main() {
	log.SetFlags(log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := agentd.Run(ctx); err != nil {
		log.Fatalf("[agent] %v", err)
	}
}

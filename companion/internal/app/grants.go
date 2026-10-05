package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/approval"
	"github.com/dotpopo/mcp-lane/companion/internal/cmdgate"
	"github.com/dotpopo/mcp-lane/companion/internal/mcpserver"
	"github.com/dotpopo/mcp-lane/companion/internal/txn"
)

// grantRecorder writes down the standing authorizations a yes carries —
// commands in this workspace, page snapshots of this workspace, this agent
// here for a while — at the moment the user gives it.
//
// The tools record the same grants when their call is still waiting for the
// answer. But a call is not always still there: a platform gives a tool a
// minute or so, and a person can take longer. Then Approve returns "pending",
// the platform moves on, and the yes that arrives later reaches the audit
// log and nothing else — the folder is asked about again next time, after
// the prompt said it would not be. Seen on 2026-09-19: one workspace asked
// three times in a day for page snapshots. Recording on the answer itself
// closes that gap; the tool-side write becomes a harmless repeat.
func grantRecorder(gate *cmdgate.Gate, snapshots mcpserver.PageSnapshots, delegations *cmdgate.Delegations, log *slog.Logger) func(*approval.Pending, bool, time.Duration) {
	return func(p *approval.Pending, approved bool, _ time.Duration) {
		req := p.Request
		if !approved || !req.Grant || req.WorkspaceID == "" {
			return
		}
		var err error
		switch {
		case req.Kind == txn.KindDelegation:
			if delegations != nil && len(req.Command) > 0 {
				delegations.Grant(req.WorkspaceID, req.Command[0])
			}
		case req.Kind == txn.KindCommand && req.Rule == mcpserver.SnapshotRule:
			if snapshots != nil {
				err = snapshots.Grant(req.WorkspaceID)
			}
		case req.Kind == txn.KindCommand:
			if gate != nil {
				err = gate.Grant(context.Background(), req.WorkspaceID)
			}
		}
		if err != nil {
			log.Error("recording a grant the user gave", "kind", req.Kind, "workspace_id", req.WorkspaceID, "err", err)
		}
	}
}

// chainDecisions runs the decision hooks in order; each sees every decision.
func chainDecisions(hooks ...func(*approval.Pending, bool, time.Duration)) func(*approval.Pending, bool, time.Duration) {
	return func(p *approval.Pending, approved bool, wait time.Duration) {
		for _, h := range hooks {
			if h != nil {
				h(p, approved, wait)
			}
		}
	}
}

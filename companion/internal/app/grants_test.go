package app

import (
	"context"
	"log/slog"
	"testing"

	"github.com/dotpopo/mcp-lane/companion/internal/approval"
	"github.com/dotpopo/mcp-lane/companion/internal/cmdgate"
	"github.com/dotpopo/mcp-lane/companion/internal/mcpserver"
	"github.com/dotpopo/mcp-lane/companion/internal/pagesnap"
	"github.com/dotpopo/mcp-lane/companion/internal/store"
	"github.com/dotpopo/mcp-lane/companion/internal/txn"
)

type grantSinks struct {
	svc   *approval.Service
	st    *store.Store
	gate  *cmdgate.Gate
	snaps *pagesnap.Grants
	dels  *cmdgate.Delegations
}

func newGrantSinks(t *testing.T) grantSinks {
	t.Helper()
	st := testStore(t)
	s := grantSinks{
		st:    st,
		gate:  cmdgate.New(st, string(cmdgate.Workspace), nil),
		snaps: pagesnap.NewGrants(),
		dels:  cmdgate.NewDelegations(),
	}
	svc, err := approval.New(approval.ModeSafe, approval.DefaultBudgets(), func(*approval.Pending) {})
	if err != nil {
		t.Fatal(err)
	}
	svc.OnDecision = grantRecorder(s.gate, &pagesnap.Service{Grants: s.snaps}, s.dels, slog.Default())
	s.svc = svc
	return s
}

// workspace makes the row a grant is keyed to; command grants are a foreign
// key into it.
func (s grantSinks) workspace(t *testing.T, id string) {
	t.Helper()
	w := &store.Workspace{ID: id, Name: "w", RootPath: t.TempDir(), Mode: store.ModeReadWrite, Status: "active"}
	if err := s.st.CreateWorkspace(context.Background(), w); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
}

// askAndLeave raises the prompt from a caller that has already gone — the
// platform's tool call timed out before the user got to the window.
func (s grantSinks) askAndLeave(t *testing.T, req *txn.ApprovalRequest) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := s.svc.Approve(ctx, req)
	if err != nil {
		t.Fatalf("Approve(%s): %v", req.ChangeSetID, err)
	}
	if !d.Pending {
		t.Fatalf("Approve(%s) = %+v, want pending: the caller is gone", req.ChangeSetID, d)
	}
}

// The three standing grants, each asked by a call that has already left by
// the time the user answers. Seen on a real machine on 2026-09-19: one
// workspace was asked for page snapshots three times in a day, because the
// grant was only ever recorded by the call that raised the prompt.
func TestAYesThatArrivesLateStillRecordsTheGrant(t *testing.T) {
	s := newGrantSinks(t)
	const ws = "ws_late"
	s.workspace(t, ws)
	reqs := []*txn.ApprovalRequest{
		{ChangeSetID: "cmd-grant:" + ws, WorkspaceID: ws, Kind: txn.KindCommand, Command: []string{"pnpm", "test"}, Grant: true},
		{ChangeSetID: "snap-grant:" + ws, WorkspaceID: ws, Kind: txn.KindCommand, Rule: mcpserver.SnapshotRule, Command: []string{"page_snapshot", "http://localhost:3000"}, Grant: true},
		{ChangeSetID: "agent:" + ws, WorkspaceID: ws, Kind: txn.KindDelegation, Command: []string{"codex", "fix the tests"}, Grant: true},
	}
	for _, req := range reqs {
		s.askAndLeave(t, req)
		if !s.svc.Resolve(req.ChangeSetID, true, "user_approved") {
			t.Fatalf("Resolve(%s) found no prompt to answer", req.ChangeSetID)
		}
	}

	if _, err := s.st.CommandGrantFor(context.Background(), ws); err != nil {
		t.Errorf("command grant not recorded: %v", err)
	}
	if !s.snaps.Granted(ws) {
		t.Error("snapshot grant not recorded")
	}
	if !s.dels.Granted(ws, "codex") {
		t.Error("delegation not recorded")
	}
}

// A no records nothing, and neither does a yes to a prompt that carried no
// grant — approving one command is not authorizing the folder.
func TestOnlyAYesWithAGrantRecordsAnything(t *testing.T) {
	s := newGrantSinks(t)
	const ws = "ws_no"
	s.workspace(t, ws)
	cases := []struct {
		req *txn.ApprovalRequest
		yes bool
	}{
		{&txn.ApprovalRequest{ChangeSetID: "cmd-grant:" + ws, WorkspaceID: ws, Kind: txn.KindCommand, Command: []string{"pnpm", "test"}, Grant: true}, false},
		{&txn.ApprovalRequest{ChangeSetID: "cmd:one", WorkspaceID: ws, Kind: txn.KindCommand, Command: []string{"rm", "-r", "x"}}, true},
		{&txn.ApprovalRequest{ChangeSetID: "snap-grant:" + ws, WorkspaceID: ws, Kind: txn.KindCommand, Rule: mcpserver.SnapshotRule, Command: []string{"page_snapshot", "http://localhost:3000"}, Grant: true}, false},
		{&txn.ApprovalRequest{ChangeSetID: "agent:" + ws, WorkspaceID: ws, Kind: txn.KindDelegation, Command: []string{"codex", "x"}, Grant: true}, false},
	}
	for _, c := range cases {
		s.askAndLeave(t, c.req)
		s.svc.Resolve(c.req.ChangeSetID, c.yes, "test")
	}
	if _, err := s.st.CommandGrantFor(context.Background(), ws); err == nil {
		t.Error("a no, or a yes without a grant, recorded a command grant")
	}
	if s.snaps.Granted(ws) {
		t.Error("a no recorded a snapshot grant")
	}
	if s.dels.Granted(ws, "codex") {
		t.Error("a no recorded a delegation")
	}
}

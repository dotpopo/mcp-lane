package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dotpopo/mcp-lane/companion/internal/cmdexec"
	"github.com/dotpopo/mcp-lane/companion/internal/cmdgate"
	"github.com/dotpopo/mcp-lane/companion/internal/nextstep"
	"github.com/dotpopo/mcp-lane/companion/internal/pagesnap"
	"github.com/dotpopo/mcp-lane/companion/internal/txn"
)

// fakeSnapshots stands in for the browser: the browser path itself is
// covered against a real one in pagesnap.
type fakeSnapshots struct {
	*pagesnap.Grants
	owner   error
	takeErr error
	every   bool
	shot    pagesnap.Shot

	mu    sync.Mutex
	takes []pagesnap.Request
}

func newFakeSnapshots() *fakeSnapshots {
	return &fakeSnapshots{Grants: pagesnap.NewGrants(), shot: pagesnap.Shot{
		JPEG: []byte{0xff, 0xd8, 0xff, 0xd9}, Width: 1280, Height: 800,
		URL: "http://localhost:5173/login", HTTPStatus: 200, Title: "Login", Settled: true,
	}}
}

func (f *fakeSnapshots) Owner(context.Context, string, int) error { return f.owner }
func (f *fakeSnapshots) AskEveryTime() bool                       { return f.every }

func (f *fakeSnapshots) Take(_ context.Context, req pagesnap.Request) (*pagesnap.Shot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takes = append(f.takes, req)
	if f.takeErr != nil {
		return nil, f.takeErr
	}
	shot := f.shot
	return &shot, nil
}

func (f *fakeSnapshots) taken() []pagesnap.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pagesnap.Request(nil), f.takes...)
}

func snapshotFixture(t *testing.T, decision txn.Decision) (*execFixture, *fakeSnapshots) {
	t.Helper()
	f := newExecFixtureAt(t, decision, cmdgate.Workspace)
	snaps := newFakeSnapshots()
	f.tools.snapshots = snaps
	return f, snaps
}

func snapshotCall(t *testing.T, f *execFixture, in pageSnapshotInput) (*mcp.CallToolResult, pageSnapshotOutput) {
	t.Helper()
	res, out, err := f.tools.pageSnapshot(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("pageSnapshot(%+v): %v", in, err)
	}
	return res, out
}

func TestPageSnapshotAsksOnceAWorkspaceAndSendsThePictureWithItsReading(t *testing.T) {
	f, snaps := snapshotFixture(t, txn.Decision{Approved: true})
	snaps.shot.Errors = []string{"TypeError: x is undefined"}
	snaps.shot.Blocked = []string{"127.0.0.1:3001"}

	res, out := snapshotCall(t, f, pageSnapshotInput{Port: 5173, Path: "/login"})
	if out.Status != "ok" || out.ImageBytes != 4 || out.HTTPStatus != 200 {
		t.Fatalf("out %+v", out)
	}
	reqs := f.approver.requests()
	if len(reqs) != 1 || reqs[0].ChangeSetID != "snap-grant:"+reqs[0].WorkspaceID || reqs[0].Rule != SnapshotRule || reqs[0].Reason != SnapshotWarning {
		t.Fatalf("approval requests %+v", reqs)
	}
	if len(res.Content) != 2 {
		t.Fatalf("content %+v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first block is %T", res.Content[0])
	}
	// The model on Claude reads only these blocks, so the reading of the
	// picture has to be here, not only in the structured output.
	for _, want := range []string{"http://localhost:5173/login", "1280×800", "HTTP 200", `"Login"`, "TypeError: x is undefined", "127.0.0.1:3001"} {
		if !strings.Contains(text.Text, want) {
			t.Errorf("summary lacks %q: %s", want, text.Text)
		}
	}
	if img, ok := res.Content[1].(*mcp.ImageContent); !ok || img.MIMEType != "image/jpeg" || len(img.Data) != 4 {
		t.Fatalf("second block %+v", res.Content[1])
	}

	takes := snaps.taken()
	if len(takes) != 1 || takes[0].Port != 5173 || takes[0].Path != "/login" || takes[0].MaxBytes != maxSnapshotBytes || takes[0].WorkspaceID != reqs[0].WorkspaceID {
		t.Fatalf("takes %+v", takes)
	}
	audit := f.audit.all()
	if len(audit) != 1 || audit[0].Outcome != cmdexec.OutcomeOK || strings.Join(audit[0].Argv, " ") != "page_snapshot http://localhost:5173/login" {
		t.Fatalf("audit %+v", audit)
	}

	snapshotCall(t, f, pageSnapshotInput{Port: 5173, Path: "/settings"})
	if n := len(f.approver.requests()); n != 1 {
		t.Fatalf("a granted workspace was asked again: %d requests", n)
	}
	if len(snaps.taken()) != 2 {
		t.Fatal("the second snapshot was not taken")
	}
}

func TestPageSnapshotOnTheStrictRungAsksForEveryPicture(t *testing.T) {
	f, snaps := snapshotFixture(t, txn.Decision{Approved: true})
	snaps.every = true
	snapshotCall(t, f, pageSnapshotInput{Port: 5173})
	snapshotCall(t, f, pageSnapshotInput{Port: 5173})
	reqs := f.approver.requests()
	if len(reqs) != 2 || !strings.HasPrefix(reqs[0].ChangeSetID, "snap:claude:") {
		t.Fatalf("strict rung requests %+v", reqs)
	}
	if len(snaps.List()) != 0 {
		t.Fatal("a strict-rung yes was remembered as a workspace grant")
	}
}

func TestAPortThatIsNotTheWorkspacesIsRefusedBeforeAnyoneIsAsked(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{pagesnap.ErrNotThisWorkspace, "not started here"},
		{pagesnap.ErrNothingListening, "start the dev server"},
	} {
		f, snaps := snapshotFixture(t, txn.Decision{Approved: true})
		snaps.owner = tc.err
		_, out := snapshotCall(t, f, pageSnapshotInput{Port: 8080})
		if out.Status != "unavailable" || out.Next != nextstep.FixInput || !strings.Contains(out.Action, tc.want) {
			t.Fatalf("%v: out %+v", tc.err, out)
		}
		if len(f.approver.requests()) != 0 || len(snaps.taken()) != 0 {
			t.Fatalf("%v: asked %d, took %d", tc.err, len(f.approver.requests()), len(snaps.taken()))
		}
	}
}

func TestADeclinedSnapshotTakesNothingGrantsNothingAndIsAudited(t *testing.T) {
	f, snaps := snapshotFixture(t, txn.Decision{Reason: "user_rejected"})
	_, out := snapshotCall(t, f, pageSnapshotInput{Port: 5173})
	if out.Status != "refused" || out.Next != nextstep.Stop {
		t.Fatalf("out %+v", out)
	}
	if len(snaps.taken()) != 0 || len(snaps.List()) != 0 {
		t.Fatal("a declined snapshot took a picture or granted the workspace")
	}
	audit := f.audit.all()
	if len(audit) != 1 || audit[0].Outcome != cmdexec.OutcomeRefused || audit[0].Rule != SnapshotRule {
		t.Fatalf("audit %+v", audit)
	}

	f, snaps = snapshotFixture(t, txn.Decision{Pending: true})
	_, out = snapshotCall(t, f, pageSnapshotInput{Port: 5173})
	if out.Status != "pending_approval" || out.Next != nextstep.AskUser || len(snaps.taken()) != 0 || len(snaps.List()) != 0 {
		t.Fatalf("pending: %+v", out)
	}
}

func TestPageSnapshotRedactsWhatThePageSaysAndAuditsAFailure(t *testing.T) {
	f, snaps := snapshotFixture(t, txn.Decision{Approved: true})
	snaps.shot.Title = "key AKIAIOSFODNN7EXAMPLE"
	snaps.shot.Errors = []string{"fetch failed with ghp_16C7e42F292c6912E7710c838347Ae178B4a"}
	res, out := snapshotCall(t, f, pageSnapshotInput{Port: 5173})
	text := res.Content[0].(*mcp.TextContent).Text
	for _, leaked := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_16C7e42F292c6912E7710c838347Ae178B4a"} {
		if strings.Contains(text, leaked) || strings.Contains(out.Title+strings.Join(out.PageErrors, ""), leaked) {
			t.Fatalf("%s leaked: %s / %+v", leaked, text, out)
		}
	}

	snaps.takeErr = errors.New("the page did not load: net::ERR_CONNECTION_REFUSED")
	_, out = snapshotCall(t, f, pageSnapshotInput{Port: 5173})
	if out.Status != "unavailable" || out.Next != nextstep.Reobserve {
		t.Fatalf("failure out %+v", out)
	}
	audit := f.audit.all()
	if last := audit[len(audit)-1]; last.Outcome != cmdexec.OutcomeFailed || !strings.Contains(last.Reason, "ERR_CONNECTION_REFUSED") {
		t.Fatalf("failure audit %+v", last)
	}
}

func TestPageSnapshotRefusesAPathOffTheDevServer(t *testing.T) {
	f, snaps := snapshotFixture(t, txn.Decision{Approved: true})
	for _, in := range []pageSnapshotInput{{Port: 5173, Path: "//evil.example/"}, {Port: 0}, {Port: 5173, Path: "http://evil.example/"}} {
		if _, _, err := f.tools.pageSnapshot(context.Background(), nil, in); err == nil {
			t.Errorf("%+v accepted", in)
		}
	}
	if len(f.approver.requests()) != 0 || len(snaps.taken()) != 0 {
		t.Fatal("a bad request reached approval or the browser")
	}
}

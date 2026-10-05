package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dotpopo/mcp-lane/companion/internal/cmdexec"
	"github.com/dotpopo/mcp-lane/companion/internal/nextstep"
	"github.com/dotpopo/mcp-lane/companion/internal/pagesnap"
	"github.com/dotpopo/mcp-lane/companion/internal/redact"
	"github.com/dotpopo/mcp-lane/companion/internal/txn"
	"github.com/dotpopo/mcp-lane/companion/internal/workspace"
)

// page_snapshot lets a model see the page it just changed, instead of
// guessing. What it may look at is narrow on purpose: a page served by a
// process a command in this workspace started, opened in a browser whose
// every request is checked, with nothing of the user's own browser in it.

// SnapshotRule is the rule id the desktop prompt keys off. It is not a
// cmdrule entry: nothing here is a command the caller chose to run.
const SnapshotRule = "page-snapshot-here"

// SnapshotWarning is shown with the prompt: the picture is not the only thing
// that leaves, and what the page shows is not always what the user expects.
const SnapshotWarning = "a picture of this page, with its title and errors, goes to the AI platform — and so does anything the page shows, such as test accounts or keys"

// maxSnapshotBytes is the heaviest picture sent. All three platforms took a
// 740 KB image in T-T2; this leaves room, and a real page at 1280×800 is far
// below it.
const maxSnapshotBytes = 512 << 10

// PageSnapshots backs page_snapshot. Implemented by *pagesnap.Service.
type PageSnapshots interface {
	Owner(ctx context.Context, workspaceID string, port int) error
	Take(ctx context.Context, req pagesnap.Request) (*pagesnap.Shot, error)
	Granted(workspaceID string) bool
	Grant(workspaceID string) error
	AskEveryTime() bool
}

type pageSnapshotInput struct {
	Port     int    `json:"port" jsonschema:"The port the dev server listens on, as printed by the command that started it."`
	Path     string `json:"path,omitempty" jsonschema:"Page path on the dev server, starting with /, with any query. Default /."`
	Width    int    `json:"width,omitempty" jsonschema:"Viewport width in CSS pixels, 320-1920 (default 1280)."`
	Height   int    `json:"height,omitempty" jsonschema:"Viewport height in CSS pixels, 240-1200 (default 800)."`
	FullPage bool   `json:"full_page,omitempty" jsonschema:"Capture the whole page, up to 6000 px tall, instead of the viewport."`

	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"Opaque workspace identifier from workspace_info."`
}

type pageSnapshotOutput struct {
	Status     string `json:"status" jsonschema:"ok | pending_approval | refused | unavailable"`
	URL        string `json:"url,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Title      string `json:"title,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	ImageBytes int    `json:"image_bytes,omitempty"`
	// StillLoading says the page had not gone quiet when it was taken.
	StillLoading bool `json:"still_loading,omitempty"`
	// Blocked names what the page tried to reach and was not allowed to.
	Blocked []string `json:"blocked,omitempty"`
	// PageErrors are uncaught exceptions and console errors.
	PageErrors []string      `json:"page_errors,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Action     string        `json:"action,omitempty"`
	Next       nextstep.Step `json:"next,omitempty" jsonschema:"What to do next, as a fixed value: ask_user | stop | fix_input | reconcile | reobserve."`
}

func (t *toolset) pageSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in pageSnapshotInput) (*mcp.CallToolResult, pageSnapshotOutput, error) {
	var zero pageSnapshotOutput
	pageURL, err := pagesnap.PageURL(in.Port, in.Path)
	if err != nil {
		return nil, zero, err
	}
	ws, err := t.open(ctx, in.WorkspaceID)
	if err != nil {
		return nil, zero, err
	}

	// Whose port it is is settled before anyone is asked: the user should
	// never be asked to approve a picture of something this tool will refuse.
	if err := t.snapshots.Owner(ctx, ws.ID(), in.Port); err != nil {
		return nil, snapshotFailure(pageURL, err), nil
	}
	if out, ok, err := t.confirmSnapshot(ctx, ws, pageURL); err != nil || !ok {
		return nil, out, err
	}

	started := time.Now()
	shot, err := t.snapshots.Take(ctx, pagesnap.Request{
		WorkspaceID: ws.ID(),
		Port:        in.Port,
		Path:        in.Path,
		Width:       in.Width,
		Height:      in.Height,
		FullPage:    in.FullPage,
		Network:     ws.Network(),
		MaxBytes:    maxSnapshotBytes,
	})
	rec := cmdexec.Record{
		WorkspaceID: ws.ID(),
		Argv:        []string{"page_snapshot", pageURL},
		Provider:    t.provider,
		Outcome:     cmdexec.OutcomeOK,
		Duration:    time.Since(started),
	}
	if err != nil {
		rec.Outcome, rec.Reason, rec.ExitCode = cmdexec.OutcomeFailed, err.Error(), -1
		t.execAudit.ExecAttempt(ctx, rec)
		return nil, snapshotFailure(pageURL, err), nil
	}
	t.execAudit.ExecAttempt(ctx, rec)

	out := pageSnapshotOutput{
		Status:       "ok",
		URL:          shot.URL,
		HTTPStatus:   shot.HTTPStatus,
		Title:        redact.Text(shot.Title),
		Width:        shot.Width,
		Height:       shot.Height,
		ImageBytes:   len(shot.JPEG),
		StillLoading: !shot.Settled,
		Blocked:      shot.Blocked,
	}
	for _, e := range shot.Errors {
		out.PageErrors = append(out.PageErrors, redact.Text(e))
	}
	// Claude hands the model only the content blocks, not the structured
	// output, so everything the picture needs to be read correctly is in the
	// text as well.
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: snapshotSummary(out)},
		&mcp.ImageContent{Data: shot.JPEG, MIMEType: "image/jpeg"},
	}}, out, nil
}

func snapshotSummary(out pageSnapshotOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Snapshot of %s: %d×%d JPEG, %d KiB", out.URL, out.Width, out.Height, (out.ImageBytes+1023)/1024)
	if out.HTTPStatus != 0 {
		fmt.Fprintf(&b, ", HTTP %d", out.HTTPStatus)
	}
	if out.Title != "" {
		fmt.Fprintf(&b, ", title %q", out.Title)
	}
	b.WriteString(".")
	if out.StillLoading {
		b.WriteString(" The page was still loading when it was taken.")
	}
	if len(out.PageErrors) > 0 {
		b.WriteString("\nPage errors:")
		for _, e := range out.PageErrors {
			b.WriteString("\n- " + e)
		}
	}
	if len(out.Blocked) > 0 {
		b.WriteString("\nThe page tried to reach these and was not allowed (only this workspace's own dev servers, and public addresses when the workspace may use the network, are reachable): " + strings.Join(out.Blocked, ", "))
	}
	return b.String()
}

// confirmSnapshot asks the local user before a workspace's pages are first
// sent, or before every one on the strict rung.
func (t *toolset) confirmSnapshot(ctx context.Context, ws *workspace.Workspace, pageURL string) (pageSnapshotOutput, bool, error) {
	every := t.snapshots.AskEveryTime()
	if !every && t.snapshots.Granted(ws.ID()) {
		return pageSnapshotOutput{}, true, nil
	}
	if t.approver == nil {
		return pageSnapshotOutput{}, false, fmt.Errorf("a page snapshot needs local approval, but no approver is configured")
	}
	// The one-time question is keyed to the workspace, like the workspace
	// command grant: whichever page raised it, the user is answering for the
	// folder. On the strict rung each picture is its own question.
	key := "snap-grant:" + ws.ID()
	if every {
		key = "snap:" + t.provider + ":" + ws.ID() + ":" + pageURL
	}
	verdict, err := t.approver.Approve(ctx, &txn.ApprovalRequest{
		ChangeSetID:   key,
		WorkspaceID:   ws.ID(),
		WorkspaceName: ws.Name(),
		Provider:      t.provider,
		Summary:       "take a picture of " + pageURL + " and send it to the AI",
		Kind:          txn.KindCommand,
		Command:       []string{"page_snapshot", pageURL},
		Rule:          SnapshotRule,
		Reason:        SnapshotWarning,
		// The one-time question is a grant like the command gate's: the
		// yes is recorded when the user gives it, not only when this call
		// is still around to hear it.
		Grant: !every,
	})
	if err != nil {
		return pageSnapshotOutput{}, false, err
	}
	switch {
	case verdict.Approved:
		if !every {
			// Recorded after the answer, and a grant that cannot be written
			// down fails the call: the prompt said this folder would not be
			// asked about again.
			if err := t.snapshots.Grant(ws.ID()); err != nil {
				return pageSnapshotOutput{}, false, fmt.Errorf("recording the snapshot authorization: %w", err)
			}
		}
		return pageSnapshotOutput{}, true, nil
	case verdict.Pending:
		return pageSnapshotOutput{
			Status: "pending_approval",
			URL:    pageURL,
			Reason: SnapshotWarning,
			Action: "the snapshot is waiting for local user approval; call page_snapshot again with the same arguments to check the outcome",
			Next:   nextstep.AskUser,
		}, false, nil
	default:
		t.execAudit.ExecAttempt(ctx, cmdexec.Record{
			WorkspaceID: ws.ID(),
			Argv:        []string{"page_snapshot", pageURL},
			Provider:    t.provider,
			Outcome:     cmdexec.OutcomeRefused,
			Reason:      verdict.Reason,
			Rule:        SnapshotRule,
		})
		return pageSnapshotOutput{
			Status: "refused",
			URL:    pageURL,
			Reason: verdict.Reason,
			Action: "the local user declined to send pictures of this workspace's pages; do not retry without being asked to",
			Next:   nextstep.Stop,
		}, false, nil
	}
}

func snapshotFailure(pageURL string, err error) pageSnapshotOutput {
	out := pageSnapshotOutput{Status: "unavailable", URL: pageURL, Reason: err.Error()}
	switch {
	case errors.Is(err, pagesnap.ErrNothingListening):
		out.Action = "start the dev server with run_command first, with a timeout long enough to keep it running; it moves to the background, and task_status shows the port it prints"
		out.Next = nextstep.FixInput
	case errors.Is(err, pagesnap.ErrNotThisWorkspace):
		out.Action = "this port is held by a process that was not started here; start the dev server with run_command in this workspace and use the port it prints"
		out.Next = nextstep.FixInput
	case errors.Is(err, pagesnap.ErrTooLarge):
		out.Action = "take it without full_page, or with a smaller viewport"
		out.Next = nextstep.FixInput
	case errors.Is(err, pagesnap.ErrUnsupported):
		out.Action = "page snapshots are not available on this machine"
		out.Next = nextstep.Stop
	default:
		out.Action = "the snapshot could not be taken; check the dev server's output with task_status and try again"
		out.Next = nextstep.Reobserve
	}
	return out
}

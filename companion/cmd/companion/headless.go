package main

// Headless (non-GUI) setup and approval commands for agents and scripts.
//
// init registers a workspace folder without any desktop window; approvals,
// approve and reject answer write/command prompts through the running
// daemon's loopback control API (the same Resolve the desktop uses, never a
// second approval semantics). pair grows --non-interactive/--ttl/--json so
// the relay pairing step is scriptable too.
//
// Machine output contract: --json prints exactly one JSON document to
// stdout (exit 0) and any human explanation to stderr. Without --json the
// human lines go to stdout, plus one trailing KEY=VALUE line for pair.
// Secrets (pairing codes, tokens) only ever appear on those two surfaces —
// never in logs.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/app"
	"github.com/dotpopo/mcp-lane/companion/internal/store"
	"github.com/dotpopo/mcp-lane/companion/internal/workspace"
)

// Exit codes for the headless commands (documented in docs/headless.md).
const (
	exitOK       = 0
	exitFailure  = 1 // operational failure: serve not running, relay refused, ...
	exitUsage    = 2 // bad flags or arguments
	exitNotFound = 3 // the named object is unknown (approval id, ...)
)

// exitError carries a process exit code through main's generic error path.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageErr(format string, args ...any) *exitError {
	return &exitError{code: exitUsage, err: fmt.Errorf(format, args...)}
}

func notFoundErr(format string, args ...any) *exitError {
	return &exitError{code: exitNotFound, err: fmt.Errorf(format, args...)}
}

// exitCodeOf maps a command error to its process exit code.
func exitCodeOf(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return exitFailure
}

// resolveDataDir honors an explicit -data-dir flag, then FYLANE_DATA_DIR,
// then the default data directory (same convention as serve).
func resolveDataDir(flagDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	if dir := os.Getenv("FYLANE_DATA_DIR"); dir != "" {
		return dir, nil
	}
	return app.DefaultDataDir()
}

// emitJSON prints one JSON document to stdout. Human text, if any, must
// already have gone to stderr — the two streams stay separable for scripts.
func emitJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding output: %w", err)
	}
	fmt.Println(string(raw))
	return nil
}

// humanf prints a human line: to stderr in --json mode (stdout carries only
// the document), otherwise to stdout like the older subcommands.
func humanf(asJSON bool, format string, args ...any) {
	if asJSON {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	} else {
		fmt.Printf(format+"\n", args...)
	}
}

// askCreate prompts for creating a missing workspace directory. Anything
// that is not a plain yes — including end of input, which is what a pipe
// gives — is a no, so a scripted run never creates folders nobody watched.
func askCreate(in io.Reader, out io.Writer, dir string) bool {
	fmt.Fprintf(out, "directory %q does not exist; create it? [y/N] ", dir)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// initCmd implements `companion init --workspace DIR [--non-interactive]`.
// It is idempotent: an already-registered folder is only verified and
// selected again, and a second run still exits 0.
func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	workspaceFlag := fs.String("workspace", "", "workspace directory to register and select")
	dataDirFlag := fs.String("data-dir", "", "data directory (default: $FYLANE_DATA_DIR or user config dir + /fylane)")
	approvalMode := fs.String("approval-mode", "", "approval policy for the fresh config: safe or balanced (default: safe; only written when no config exists)")
	nonInteractive := fs.Bool("non-interactive", false, "never prompt; fail instead of asking (scripts should always pass this)")
	asJSON := fs.Bool("json", false, "print one JSON document to stdout, human text to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := *workspaceFlag
	if dir == "" {
		if fs.NArg() == 1 {
			dir = fs.Arg(0)
		} else if fs.NArg() > 1 {
			return usageErr("init takes at most one directory, got %d arguments", fs.NArg())
		}
	}
	if dir == "" {
		return usageErr("init requires --workspace <dir>")
	}
	switch *approvalMode {
	case "", "safe", "balanced":
	default:
		return usageErr("unknown -approval-mode %q (safe or balanced)", *approvalMode)
	}
	dataDir, err := resolveDataDir(*dataDirFlag)
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot use %q: %w", abs, err)
		}
		if *nonInteractive {
			return fmt.Errorf("workspace %q does not exist (non-interactive: create it first or rerun without the flag to be asked)", abs)
		}
		if !askCreate(os.Stdin, os.Stderr, abs) {
			return &exitError{code: exitFailure, err: fmt.Errorf("workspace %q does not exist", abs)}
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("creating workspace %q: %w", abs, err)
		}
		info, err = os.Stat(abs)
		if err != nil {
			return fmt.Errorf("cannot use %q: %w", abs, err)
		}
	}
	if !info.IsDir() {
		return usageErr("cannot use %q: it is a file, and a workspace is a folder", abs)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	// Minimal config: a fresh data dir gets the strict default policy.
	// An existing config is never rewritten — init is idempotent, not a
	// reset.
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); os.IsNotExist(err) {
		mode := *approvalMode
		if mode == "" {
			mode = "safe"
		}
		if err := app.SaveApprovalMode(dataDir, mode); err != nil {
			return fmt.Errorf("writing minimal config: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(dataDir, "fylane.db"))
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer st.Close()
	manager, err := workspace.NewManager(st, dataDir)
	if err != nil {
		return err
	}
	rec, err := manager.Add(ctx, abs)
	already := false
	if err != nil {
		if !errors.Is(err, workspace.ErrDuplicateRoot) {
			return fmt.Errorf("registering workspace: %w", err)
		}
		already = true
		rec, err = findWorkspaceByRoot(ctx, manager, abs)
		if err != nil {
			return err
		}
	}
	if err := manager.SetCurrent(ctx, rec.ID); err != nil {
		return fmt.Errorf("selecting workspace: %w", err)
	}

	humanf(*asJSON, "workspace ready: %s (%s) at %s", rec.Name, rec.ID, rec.RootPath)
	if already {
		humanf(*asJSON, "already registered; verified and selected again")
	}
	humanf(*asJSON, "next: pair this device, then serve it:")
	humanf(*asJSON, "  fylane-companion pair -relay <url> -data-dir %s", dataDir)
	humanf(*asJSON, "  fylane-companion serve -data-dir %s", dataDir)
	if *asJSON {
		return emitJSON(map[string]any{
			"data_dir": dataDir, "workspace_id": rec.ID,
			"path": rec.RootPath, "name": rec.Name,
			"already_registered": already, "current_workspace_id": rec.ID,
		})
	}
	return nil
}

// findWorkspaceByRoot locates a registered workspace by its real path (the
// Add-duplicate path reports no record, so look it up for idempotency).
func findWorkspaceByRoot(ctx context.Context, manager *workspace.Manager, root string) (*store.Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace root: %w", err)
	}
	list, err := manager.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range list {
		if w.RootPath == real {
			return w, nil
		}
	}
	return nil, fmt.Errorf("workspace for %q not found", filepath.Base(root))
}

// controlClient dials the running daemon's loopback control API using the
// rendezvous file serve wrote (<data-dir>/control.json). The file holds a
// session-scoped bearer token with 0600 permissions.
type controlClient struct {
	addr  string
	token string
	http  *http.Client
}

func dialControl(dataDir string) (*controlClient, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "control.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no control file in %s: is `serve` running with the same -data-dir?", dataDir)
		}
		return nil, fmt.Errorf("reading control file: %w", err)
	}
	var doc struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing control file: %w", err)
	}
	if doc.Addr == "" || doc.Token == "" {
		return nil, fmt.Errorf("control file is incomplete; restart `serve`")
	}
	return &controlClient{addr: doc.Addr, token: doc.Token,
		http: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (c *controlClient) get(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+c.addr+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("contacting the daemon at %s: %w", c.addr, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

func (c *controlClient) post(path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = strings.NewReader(string(raw))
	} else {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+c.addr+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("contacting the daemon at %s: %w", c.addr, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, out, nil
}

// pendingApproval mirrors the control API's approval view (only the fields
// the table and the JSON passthrough need; unknown fields are preserved by
// decoding into maps at the JSON boundary).
type pendingApproval struct {
	ChangeSetID   string `json:"change_set_id"`
	WorkspaceName string `json:"workspace_name"`
	Provider      string `json:"provider"`
	Summary       string `json:"summary"`
	Kind          string `json:"kind"`
}

// approvalsCmd implements `companion approvals [--json]`.
func approvalsCmd(args []string) error {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	dataDirFlag := fs.String("data-dir", "", "data directory (default: $FYLANE_DATA_DIR or user config dir + /fylane)")
	asJSON := fs.Bool("json", false, "print one JSON document to stdout, human text to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr("approvals takes no arguments")
	}
	dataDir, err := resolveDataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	ctl, err := dialControl(dataDir)
	if err != nil {
		return err
	}
	status, body, err := ctl.get("/v1/approvals")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("listing approvals: daemon answered %d: %s", status, strings.TrimSpace(string(body)))
	}
	var doc struct {
		Approvals []pendingApproval `json:"approvals"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("parsing approvals: %w", err)
	}
	if *asJSON {
		// Passthrough of the decoded document keeps new view fields
		// available to scripts without this command naming each one.
		var raw any
		if err := json.Unmarshal(body, &raw); err != nil {
			return fmt.Errorf("parsing approvals: %w", err)
		}
		humanf(true, "%d pending approval(s)", len(doc.Approvals))
		return emitJSON(raw)
	}
	if len(doc.Approvals) == 0 {
		fmt.Println("no pending approvals")
		return nil
	}
	fmt.Printf("%-28s %-10s %-12s %s\n", "CHANGE_SET_ID", "KIND", "WORKSPACE", "SUMMARY")
	for _, p := range doc.Approvals {
		fmt.Printf("%-28s %-10s %-12s %s\n", p.ChangeSetID, p.Kind, p.WorkspaceName, p.Summary)
	}
	return nil
}

// resolveApproval implements approve/reject through the same Resolve the
// desktop window uses (POST /v1/approvals/resolve).
func resolveApproval(args []string, name string, approved bool) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dataDirFlag := fs.String("data-dir", "", "data directory (default: $FYLANE_DATA_DIR or user config dir + /fylane)")
	asJSON := fs.Bool("json", false, "print one JSON document to stdout, human text to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErr("%s requires exactly one <change-set-id>", name)
	}
	id := fs.Arg(0)
	dataDir, err := resolveDataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	ctl, err := dialControl(dataDir)
	if err != nil {
		return err
	}
	verb := "approving"
	if !approved {
		verb = "rejecting"
	}
	status, body, err := ctl.post("/v1/approvals/resolve", map[string]any{
		"change_set_id": id, "approved": approved,
	})
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, id, err)
	}
	switch status {
	case http.StatusOK:
		// The decision reason names this surface ("user_approved" by
		// default server-side); the audit row says what happened.
		past := "approved"
		if !approved {
			past = "rejected"
		}
		humanf(*asJSON, "%s %s", past, id)
		if *asJSON {
			return emitJSON(map[string]any{"resolved": true, "change_set_id": id, "approved": approved})
		}
		return nil
	case http.StatusNotFound:
		return notFoundErr("%s is unknown or already decided", id)
	default:
		return fmt.Errorf("%s %s: daemon answered %d: %s", verb, id, status, strings.TrimSpace(string(body)))
	}
}

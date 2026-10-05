// Package mcpserver exposes Fylane workspaces over MCP (Streamable HTTP).
// Tools address workspaces by opaque workspace_id; absolute paths
// never appear in any input or output.
package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dotpopo/mcp-lane/companion/internal/cmdexec"
	"github.com/dotpopo/mcp-lane/companion/internal/cmdgate"
	"github.com/dotpopo/mcp-lane/companion/internal/cmdrule"
	"github.com/dotpopo/mcp-lane/companion/internal/codeagent"
	"github.com/dotpopo/mcp-lane/companion/internal/mcpgate"
	"github.com/dotpopo/mcp-lane/companion/internal/readbox"
	"github.com/dotpopo/mcp-lane/companion/internal/routerule"
	"github.com/dotpopo/mcp-lane/companion/internal/store"
	"github.com/dotpopo/mcp-lane/companion/internal/tasks"
	"github.com/dotpopo/mcp-lane/companion/internal/txn"
	"github.com/dotpopo/mcp-lane/companion/internal/workspace"
	"github.com/dotpopo/mcp-lane/shared/authsrv"
	"github.com/dotpopo/mcp-lane/shared/buildinfo"
	"github.com/dotpopo/mcp-lane/shared/tunnel"
)

// serverVersion mirrors the stamped product version (shared/buildinfo).
var serverVersion = buildinfo.Version

// WorkspaceSource resolves workspace IDs to runtime handles. Implemented by
// *workspace.Manager; defined here so the server depends only on what it
// consumes.
type WorkspaceSource interface {
	// List returns all non-revoked workspace records.
	List(ctx context.Context) ([]*store.Workspace, error)
	// Current returns the current workspace record.
	Current(ctx context.Context) (*store.Workspace, error)
	// Open returns a runtime handle for an active workspace.
	Open(ctx context.Context, id string) (*workspace.Workspace, error)
}

// ReadApprover confirms reads of sensitive files. Implemented by
// *approval.Service.
type ReadApprover interface {
	ApproveRead(ctx context.Context, provider, workspaceID, workspaceName, path string) (txn.Decision, error)
}

// CommandRunner executes one validated command. Implemented by
// *cmdexec.Runner; defined here so the server depends only on what it calls.
type CommandRunner interface {
	Run(ctx context.Context, spec cmdexec.Spec) (*cmdexec.Result, error)
}

// TaskTracker runs work that may outlive the call that started it.
// Implemented by *tasks.Manager.
type TaskTracker interface {
	Run(ctx context.Context, meta tasks.Meta, work tasks.Work) (tasks.Snapshot, error)
	Status(id string, stdoutCursor, stderrCursor int) (tasks.Snapshot, error)
}

// CommandApprover is the local approval authority for commands the rule table
// gated. Implemented by *approval.Service.
type CommandApprover interface {
	Approve(ctx context.Context, req *txn.ApprovalRequest) (txn.Decision, error)
}

// CommandGate applies the user's approval rung and workspace grants to a rule
// verdict. Implemented by *cmdgate.Gate.
type CommandGate interface {
	Decide(ctx context.Context, workspaceID string, d cmdrule.Decision) (cmdgate.Verdict, error)
	Grant(ctx context.Context, workspaceID string) error
}

// AgentRegistry resolves the coding agent a delegated task should go to.
// Implemented by *codeagent.Registry.
type AgentRegistry interface {
	Lookup(name string) (codeagent.Agent, error)
	Installed() []string
}

// ProviderRegistry resolves the local MCP servers mcp_gateway may proxy to.
// Implemented by *mcpgate.Registry.
type ProviderRegistry interface {
	Lookup(name string) (mcpgate.Provider, error)
	List() []mcpgate.Provider
	Names() []string
}

// Deps are the collaborators the server exposes tools over. Source is
// required; Engine and Reads gate the write tools and sensitive reads —
// without them those operations fail closed.
type Deps struct {
	Source WorkspaceSource
	Engine *txn.Engine
	Reads  ReadApprover
	// Exec, Tasks, Approve, Gate, and ExecAudit back run_command and
	// task_status. All are required for the tools to appear at all: a
	// command surface missing its approver, its rung, or its audit is not a
	// reduced feature, it is a hole.
	Exec      CommandRunner
	Tasks     TaskTracker
	Approve   CommandApprover
	Gate      CommandGate
	ExecAudit cmdexec.Auditor
	// Runs lets task_status answer for a run the task table has forgotten.
	// Nil leaves task_status able to see live work only, which is what it
	// could see before the journal existed.
	Runs RunJournal
	// Activity lets workspace_info say where the last session left off,
	// from the change sets and commands on record. Nil leaves that out.
	Activity ActivityJournal
	// Memory backs the memory_* tools. Nil leaves them unregistered.
	Memory MemoryStore
	// Delegations remembers a yes to a coding agent in a workspace for a
	// bounded time (cmdgate.DelegationTTL). Nil asks on every code_task.
	Delegations DelegationGrants
	// Agents backs code_task. Nil leaves the tool unregistered — a
	// Companion with no coding agent installed should not advertise one.
	Agents AgentRegistry
	// Box is the kernel read boundary applied to every program started from
	// here. Nil is a platform without one.
	Box *readbox.Box
	// Navigators backs code_navigate. Nil, or a registry with no installed
	// language server, leaves the tool unregistered: a tool that answers
	// "unavailable" to everything is worse than one that is not there, because
	// a model will keep trying it.
	Navigators Navigators
	// Providers backs mcp_gateway. Nil, or empty, leaves the tool
	// unregistered: the gateway is opt-in by configuration, and a Companion
	// nobody configured a provider on must not advertise one.
	Providers ProviderRegistry
	// Snapshots backs page_snapshot. Nil — no browser installed, or a platform
	// that cannot name the process on a port — leaves the tool unregistered.
	Snapshots PageSnapshots
	// Rules reads the user's route rules. Only their "ask" action applies
	// here — see toolset.execute for why an MCP write is never relocated.
	Rules RuleSource
	// Seen, when set, is told which platform just called. It is what puts a
	// source on the lane's "connected" list: nothing else in the product
	// records that a platform is actually reaching this machine, and a list
	// that always reads "not connected" is worse than no list. Nil leaves the
	// list alone, which is what tests and the local listener want.
	Seen func(provider string)
	// Remotes, when set, lists the workspaces on other machines this
	// Companion reaches over ssh, so workspace_info can offer them. A call
	// carrying one of their ids never reaches this server: the router in
	// front forwards it to that machine.
	Remotes func(ctx context.Context) []RemoteWorkspace
}

// RemoteWorkspace is a workspace on another machine, as workspace_info lists
// it. Identifiers and names only — the machine's paths stay on the machine.
type RemoteWorkspace struct {
	WorkspaceID string
	Name        string
	Mode        string
	Status      string
	Machine     string
	// Current marks the current folder of the machine the window stands
	// on. At most one remote workspace carries it, and while one does the
	// local folders are listed but none of them is current.
	Current bool
}

// RunJournal recalls how a run ended. Defined at the consumer: task_status
// needs one question answered — "what happened to this id" — and nothing
// about the rest of the store.
type RunJournal interface {
	FindRun(ctx context.Context, runID string) (*store.ExecEvent, error)
}

// RuleSource hands over the current route rules. Defined at the consumer;
// nil means the tools run without them.
type RuleSource interface {
	Load() ([]routerule.Rule, error)
}

// Options configures optional server behavior.
type Options struct {
	// EnableWaitProbe registers the Phase 0 diagnostic tools: wait_probe
	// (how long a platform lets a tool call block) and
	// payload_probe (how large a tool result a platform accepts, task
	// measurement), plus image_probe (whether the model sees an image a tool
	// returns, D38). Never enabled by default.
	EnableWaitProbe bool
	// MaxInlineBytes caps content returned inline by one read; zero keeps
	// the 1 MiB default. To be calibrated per platform once the
	// payload measurements land.
	MaxInlineBytes int
	// TaskCeiling is the user's ceiling on how long a command may run
	// (settings › execution). It bounds what a caller may ask for rather than
	// providing a default a caller can raise. nil keeps cmdexec's default.
	TaskCeiling func() time.Duration
}

// New builds an MCP server exposing the workspaces of deps.Source, serving
// callers with the generic ("unknown") approval budget.
func New(deps Deps, opts *Options) *mcp.Server {
	return newWithProvider(deps, opts, "unknown")
}

// newWithProvider binds the toolset to the named calling platform so its
// approval budget applies.
func newWithProvider(deps Deps, opts *Options, provider string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "fylane-companion",
		Title:   "Fylane",
		Version: serverVersion,
	}, nil)

	tools := &toolset{src: deps.Source, engine: deps.Engine, reads: deps.Reads,
		rules: deps.Rules, provider: provider, remotes: deps.Remotes,
		exec: deps.Exec, tasks: deps.Tasks, approver: deps.Approve, gate: deps.Gate,
		execAudit: deps.ExecAudit, runs: deps.Runs, activityLog: deps.Activity, memory: deps.Memory, agents: deps.Agents, delegations: deps.Delegations,
		providers: deps.Providers, navigators: deps.Navigators, box: deps.Box, snapshots: deps.Snapshots}
	if opts != nil {
		tools.inlineBudget = opts.MaxInlineBytes
		tools.taskCeiling = opts.TaskCeiling
	}

	// Every tool result leaves here carrying its body twice. Drop the copy
	// this platform does not read; a caller we do not recognise keeps both.
	if trim, ok := wireTrims[provider]; ok {
		srv.AddReceivingMiddleware(trimDuplicateBody(trim))
	}

	srv.AddResourceTemplate(&mcp.ResourceTemplate{
		Name:        "workspace-file",
		Title:       "Workspace file",
		Description: "Text content of a workspace file. Optional ?start_line=&end_line= (1-based, inclusive) read a line range; responses are capped at the inline budget, so page with start_line.",
		MIMEType:    "text/plain",
		URITemplate: "fylane://{workspace_id}/{+path}",
	}, tools.readResource)

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	write := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workspace_info",
		Description: "Get workspace information and limits, plus the list of all available workspaces with their opaque IDs. Call this first to obtain the workspace_id required by all other tools. Entries with a machine name live on another computer; their files, commands and approvals happen there, and their workspace_id works with every tool. Absolute paths are never returned.",
		Annotations: readOnly,
	}, tools.workspaceInfo)

	if deps.Memory != nil {
		_, compacts := deps.Memory.(MemoryCompactor)
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "memory",
			Description: memoryDescription(compacts),
			// The branches disagree: recall, search and read only look,
			// note and compact write. The tool is annotated for the ones
			// that write, because an annotation a caller trusts must not
			// promise less than the tool can do.
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
		}, tools.memoryTool)
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_directory",
		Description: "List what is under a workspace path, or describe one file. Depth 1 lists immediate children; larger depths descend recursively. Point path at a file instead and the answer is that single entry with its size and SHA-256 — the hash write_file, edit_file and apply_patch want as expected_sha256, without reading the content. Excluded and sensitive paths are hidden. Examples: {} lists the workspace root · {\"path\":\"src\",\"depth\":2} · {\"path\":\"src/main.go\"} describes one file.",
		Annotations: readOnly,
	}, tools.listDirectory)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search_files",
		Description: "Search file names (glob) and/or file contents (substring or regex) inside the workspace. Returns matching paths and lines within time and result budgets.",
		Annotations: readOnly,
	}, tools.searchFiles)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read workspace text files (UTF-8, UTF-16, GBK). Give path for one file, optionally narrowed to a 1-based line range, or paths for up to 20 files in one call. Every file comes back in files[] with its content, the SHA-256 of the whole file, its encoding and a truncation flag; a file that cannot be read carries an error there and does not fail the others. Binary files return metadata with empty content. Examples: {\"path\":\"src/main.go\"} · {\"path\":\"src/main.go\",\"start_line\":120,\"end_line\":180} · {\"paths\":[\"go.mod\",\"README.md\"]}.",
		Annotations: readOnly,
	}, tools.readFiles)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_file",
		Description: "Create a new file or fully replace an existing one. Replacing requires expected_sha256 from a previous read. Writes require local user approval; a pending_approval result means retry with the returned change_set_id.",
		Annotations: write,
	}, tools.writeFile)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "apply_patch",
		Description: "Apply a unified diff to a single file. Requires expected_sha256 of the current file. Writes require local user approval.",
		Annotations: write,
	}, tools.applyPatch)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "edit_file",
		Description: "Apply precise text edits to a single file without sending a full diff: replace_exact, insert_before, insert_after, delete_exact (unique match required), replace_range (1-based lines). Requires expected_sha256 of the current file. Writes require local user approval. Examples: {\"path\":\"src/app.ts\",\"expected_sha256\":\"…\",\"edits\":[{\"type\":\"replace_exact\",\"match\":\"const port = 3000\",\"content\":\"const port = 8080\"}]} · {\"path\":\"src/app.ts\",\"expected_sha256\":\"…\",\"edits\":[{\"type\":\"insert_after\",\"match\":\"import fs from 'fs'\",\"content\":\"\\nimport path from 'path'\"},{\"type\":\"replace_range\",\"start_line\":40,\"end_line\":42,\"content\":\"  return cached\\n\"}]}.",
		Annotations: write,
	}, tools.editFile)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "change_manage",
		Description: "Manage changes with one tool, selected by action. action=apply_change_set applies multiple operations (create/update/move/delete) atomically as one approved change set (summary, operations) — preferred for multi-file edits. action=move moves or renames a file (from, to, expected_sha256). action=delete removes a file or directory into the local recycle area (path; recursive=true plus extra confirmation for non-empty directories). action=rollback reverts an applied change set within its rollback window (change_set_id); it fails with a diff if affected files changed since — never overwrites silently. All actions require local user approval; a pending_approval result means retry with the returned change_set_id. Examples: {\"action\":\"move\",\"from\":\"src/old.ts\",\"to\":\"src/new.ts\",\"expected_sha256\":\"…\"} · {\"action\":\"delete\",\"path\":\"tmp/scratch\",\"recursive\":true} · {\"action\":\"apply_change_set\",\"summary\":\"rename the port constant\",\"operations\":[{\"type\":\"update\",\"path\":\"src/app.ts\",\"content\":\"…\",\"expected_sha256\":\"…\"}]} · {\"action\":\"rollback\",\"change_set_id\":\"cs_12\"}.",
		Annotations: write,
	}, tools.changeManage)

	// Commands appear only when every part of their safety chain is present.
	if deps.Exec != nil && deps.Tasks != nil && deps.Approve != nil && deps.Gate != nil && deps.ExecAudit != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "run_command",
			Description: "Run a command inside the workspace: tests, builds, linters, git. Pass the program and its arguments as separate strings — this is not a shell, so pipes, redirection, globs, && and shell interpreters are not available. Commands that discard work, publish, or change the machine need local user approval; some are refused outright. A command that does not finish within the budget returns status=running with a task_id to poll via task_status.",
			Annotations: write,
		}, tools.runCommand)

		if deps.Agents != nil && len(deps.Agents.Installed()) > 0 {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "code_task",
				Description: "Hand a whole coding task to an agent installed on this machine (opencode, Codex) and get a task_id to follow with task_status. Use this for work too large for a sequence of run_command and edit calls. The agent does not see this conversation, so the prompt must contain everything it needs. It requires local user approval, and once approved it reads, writes, and runs commands on its own — Fylane does not review its individual steps.",
				Annotations: write,
			}, tools.codeTask)
		}

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "git_query",
			Description: "Ask git a read-only question about the workspace without approval: op=status (porcelain, with branch), diff (working tree, or staged=true for the index; ref to compare against), log (limit, ref, path), show (ref, default HEAD), blame (path, optional line range). Prefer this over run_command for anything that only reads git; it works in read-only workspaces too. Files the workspace hides are left out and counted in hidden_entries. Examples: {\"op\":\"status\"} · {\"op\":\"diff\"} · {\"op\":\"diff\",\"staged\":true} · {\"op\":\"log\",\"limit\":10,\"path\":\"src\"} · {\"op\":\"show\",\"ref\":\"HEAD~1\"} · {\"op\":\"blame\",\"path\":\"src/app.ts\",\"start_line\":40,\"end_line\":60}.",
			Annotations: readOnly,
		}, tools.gitQuery)

		if deps.Snapshots != nil {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "page_snapshot",
				Description: "See a page this workspace's dev server is serving: opens http://localhost:<port><path> in a headless browser on this machine and returns a JPEG of it, with the HTTP status, title, page errors, and any requests the page was not allowed to make. Use it after changing UI code to check the result instead of guessing. The server must have been started with run_command in this workspace — give it a timeout long enough to keep running and read its port from task_status; a port held by any other process is refused. The first snapshot in a workspace asks the local user.",
				Annotations: readOnly,
			}, tools.pageSnapshot)
		}

		mcp.AddTool(srv, &mcp.Tool{
			Name:        "task_status",
			Description: "Get the state and new output of a command started by run_command that is still going. Pass the cursors from the previous response to receive only output produced since then.",
			Annotations: readOnly,
		}, tools.taskStatus)
	}

	// code_navigate appears only when a language server is actually installed
	// and there is someone to ask about starting it. A tool that answers
	// "unavailable" to every call is worse than an absent one: a model keeps
	// trying it, and each attempt costs a round trip to learn the same thing.
	if deps.Navigators != nil && len(deps.Navigators.Registry().Names()) > 0 && deps.Approve != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "code_navigate",
			Description: "Ask a language server installed on this machine about code in the workspace: action=definition finds where a symbol is defined, action=references finds every place it is used, action=symbols lists what one file contains. Answers are exact rather than textual — a reference is a reference, not a string that looks like one. Give path plus symbol, adding line when the same name occurs more than once in the file. It does not fall back to text search: if no server handles the file, or the symbol is not there, it says so. Starting a server in a folder asks the local user once. Examples: {\"action\":\"definition\",\"path\":\"src/app.ts\",\"symbol\":\"startServer\"} · {\"action\":\"references\",\"path\":\"src/app.ts\",\"symbol\":\"port\",\"line\":42} · {\"action\":\"symbols\",\"path\":\"src/app.ts\"}. Installed servers: " + strings.Join(deps.Navigators.Registry().Names(), ", ") + ".",
			Annotations: readOnly,
		}, tools.codeNavigate)
	}

	// The local MCP gateway appears only when a provider is configured and the
	// approval chain behind it is present. A gateway without an approver would
	// forward opaque calls with nobody able to stop them, which is the one
	// thing this surface must never be.
	if deps.Providers != nil && len(deps.Providers.Names()) > 0 &&
		deps.Approve != nil && deps.Gate != nil && deps.ExecAudit != nil {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "mcp_gateway",
			Description: "Reach an MCP server installed on the user's machine. action=list_providers names the configured servers, action=list_tools returns one server's tools with their input schemas, action=call_tool forwards a call to one of them. Fylane cannot inspect what a proxied tool does, so by default every call stops for local user approval regardless of the user's approval settings; a pending_approval result means retry with the same provider and tool. Examples: {\"action\":\"list_providers\"} · {\"action\":\"list_tools\",\"provider\":\"sqlite\"} · {\"action\":\"call_tool\",\"provider\":\"sqlite\",\"tool\":\"query\",\"arguments\":{\"sql\":\"select 1\"}}.",
			Annotations: write,
		}, tools.mcpGateway)
	}

	if opts != nil && opts.EnableWaitProbe {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "wait_probe",
			Description: "Diagnostic: hold this call open for the given number of seconds, then return. Used to measure how long the platform keeps a pending tool call alive.",
			Annotations: readOnly,
		}, tools.waitProbe)
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "payload_probe",
			Description: "Diagnostic: return a text payload of the given size in MiB. Used to measure the largest tool result the platform accepts.",
			Annotations: readOnly,
		}, tools.payloadProbe)
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "image_probe",
			Description: "Diagnostic: return an image showing six digits, or check the digits read from the last image. Used to measure whether the platform lets the model see an image returned by a tool. The digits appear only in the image: read them and call again with answer set to them.",
			Annotations: readOnly,
		}, tools.imageProbe)
	}

	return srv
}

// Handler wraps the server in a Streamable HTTP handler. Stateless mode
// serves the sessionless 2026-07-28 protocol (SEP-2567/2575: ChatGPT's
// discovery pipeline requires it) while still answering older session-style
// clients with a per-request temporary session — our tools carry no
// session state, so both dialects see identical behavior.
func Handler(deps Deps, opts *Options) http.Handler {
	return mcp.NewStreamableHTTPHandler(providerPicker(deps, opts),
		&mcp.StreamableHTTPOptions{Stateless: true})
}

// knownProviders are the platforms the product knows by name. Anything
// else keeps the name its client registered under (the direct surface and
// the relay stamp that after checking the token) or falls back to
// "unknown"; either way it gets the generic approval budget.
var knownProviders = map[string]bool{"claude": true, "chatgpt": true, "grok": true, "gemini": true}

// maxNamedCallers bounds how many differently named unknown clients get a
// toolset of their own; registration is open, so the map must not grow with
// it. Past the bound a caller is served as "unknown".
const maxNamedCallers = 16

func normalizeProvider(header string) string {
	p := strings.ToLower(strings.TrimSpace(header))
	if knownProviders[p] {
		return p
	}
	if n := authsrv.CallerName(header); n != "" {
		return n
	}
	return "unknown"
}

// providerPicker returns the per-request server selector. The relay stamps
// tunnel.ProviderHeader from OAuth client metadata; the value only selects
// an approval budget, so an unexpected or missing header degrades to the
// generic budget rather than failing. Servers are cached per provider.
func providerPicker(deps Deps, opts *Options) func(*http.Request) *mcp.Server {
	var mu sync.Mutex
	servers := map[string]*mcp.Server{}
	return func(r *http.Request) *mcp.Server {
		p := normalizeProvider(r.Header.Get(tunnel.ProviderHeader))
		// Both modes land here — the relay stamps the header on the way in,
		// and direct mode stamps it in its own handler — so this is the one
		// place that sees every call whoever carried it.
		if deps.Seen != nil && knownProviders[p] {
			deps.Seen(p)
		}
		mu.Lock()
		defer mu.Unlock()
		srv, ok := servers[p]
		if !ok {
			if !knownProviders[p] && p != "unknown" && len(servers) >= len(knownProviders)+1+maxNamedCallers {
				p = "unknown"
				if srv, ok = servers[p]; ok {
					return srv
				}
			}
			srv = newWithProvider(deps, opts, p)
			servers[p] = srv
		}
		return srv
	}
}

func ptr[T any](v T) *T { return &v }

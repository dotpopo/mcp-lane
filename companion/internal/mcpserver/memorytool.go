package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dotpopo/mcp-lane/companion/internal/store"
)

// One tool, five branches. Memory was five tools of the twenty-odd this
// server offers, and a platform holds all of them in one turn alongside
// whatever else the user has connected — OpenAI asks for fewer than twenty
// at the start of a turn. Five names for one subject is where that budget
// is cheapest to win back.
//
// The branches are the handlers in memory.go and compact.go, called
// unchanged: this file only chooses between them and flattens what they
// answer into one shape. Merging costs a wider schema and a choice the
// model can get wrong, which is what the examples in the description are
// for. Field names stay distinct per branch on purpose — a list of titles
// and a list of whole notes must never arrive under the same name.

type memoryInput struct {
	Action      string `json:"action" jsonschema:"Which branch: recall, note, search, read, or compact."`
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"Opaque workspace identifier from workspace_info."`

	Title       string            `json:"title,omitempty" jsonschema:"note: one line saying what was done or learned. Up to 120 bytes."`
	Body        string            `json:"body,omitempty" jsonschema:"note: what, why, and what it means next time. Up to 2000 bytes; longer is cut."`
	ChangeSetID string            `json:"change_set_id,omitempty" jsonschema:"note: the change set this note is about, if any."`
	RunID       string            `json:"run_id,omitempty" jsonschema:"note: the task_id of the command run this note is about, if any."`
	Page        *store.MemoryPage `json:"page,omitempty" jsonschema:"note: rewrite the workspace's current-state page. The whole page is replaced: send every field that should stay."`

	Query string `json:"query,omitempty" jsonschema:"search: words to look for; every word must appear in the title or body. Up to 200 bytes."`

	IDs      []int64 `json:"ids,omitempty" jsonschema:"read: notes to return in full, up to 10."`
	BeforeID int64   `json:"before_id,omitempty" jsonschema:"read: page the trail, returning notes older than this id, from next_before_id of the previous answer."`
	Limit    int     `json:"limit,omitempty" jsonschema:"recall, search and read: how many notes. Defaults 10, 10 and 5; maximums 30, 30 and 20."`

	Steps  []planStepInput `json:"steps,omitempty" jsonschema:"plan: the whole plan in order, at most 20 steps. Writing a plan replaces the previous one."`
	StepID int64           `json:"step_id,omitempty" jsonschema:"step: which step to move, by the id the plan gives it."`
	State  string          `json:"state,omitempty" jsonschema:"step: todo, doing, blocked, or done."`
	Note   string          `json:"note,omitempty" jsonschema:"step: why it is blocked, or what it turned out to involve. Omit to leave the step's note as it is."`

	Summary   string `json:"summary,omitempty" jsonschema:"compact, second call: the summary of the notes the first call returned, up to 1500 bytes."`
	ThroughID int64  `json:"through_id,omitempty" jsonschema:"compact, second call: the through_id the first call returned; every live note up to it is archived."`
}

// planStepInput is one step on the way in. Ids are not accepted here: a
// plan is written whole and the store issues the ids, so a caller cannot
// aim a rewrite at rows of a plan that has already been replaced.
type planStepInput struct {
	Title string `json:"title" jsonschema:"What this step is. Up to 200 bytes."`
	State string `json:"state,omitempty" jsonschema:"todo, doing, blocked, or done. Defaults to todo."`
	Note  string `json:"note,omitempty" jsonschema:"Why it is blocked, or what it turned out to involve. Up to 500 bytes."`
}

type memoryOutput struct {
	Page      *store.MemoryPage `json:"page,omitempty" jsonschema:"recall: the current-state page, as last rewritten."`
	UpdatedAt time.Time         `json:"page_updated_at,omitzero"`
	UpdatedBy string            `json:"page_updated_by,omitempty"`
	Notes     []noteHead        `json:"notes,omitempty" jsonschema:"recall: the newest notes, titles only, newest first."`
	NoteCount int               `json:"note_count,omitempty" jsonschema:"How many notes the workspace has."`
	Archived  int               `json:"archived_count,omitempty" jsonschema:"recall: notes folded into a summary; still readable by id and searchable."`

	NoteID      int64    `json:"note_id,omitempty" jsonschema:"note: the id of the note just written."`
	PageUpdated bool     `json:"page_updated,omitempty"`
	Cut         []string `json:"cut,omitempty" jsonschema:"note: fields that were longer than their limit and were cut to it."`

	Plan []*store.MemoryStep `json:"plan,omitempty" jsonschema:"recall, plan and step: the plan in order, as stored."`

	Matches []memoryMatch `json:"matches,omitempty" jsonschema:"search: newest first."`

	FullNotes    []store.MemoryNote `json:"full_notes,omitempty" jsonschema:"read: whole notes, newest first."`
	NextBeforeID int64              `json:"next_before_id,omitempty" jsonschema:"read: present when older notes remain; pass as before_id."`

	ToSummarize []store.MemoryNote `json:"notes_to_summarize,omitempty" jsonschema:"compact, first call: the oldest notes, oldest first, to summarize."`
	ThroughID   int64              `json:"through_id,omitempty" jsonschema:"compact, first call: pass back with the summary."`
	ArchivedNow int                `json:"archived,omitempty" jsonschema:"compact, second call: how many notes the summary now stands for."`
	SummaryID   int64              `json:"summary_note_id,omitempty"`
	Live        int                `json:"live_notes,omitempty"`

	Hint string `json:"hint,omitempty"`
}

func (t *toolset) memoryTool(ctx context.Context, req *mcp.CallToolRequest, in memoryInput) (*mcp.CallToolResult, memoryOutput, error) {
	var zero memoryOutput
	if t.memory == nil {
		return nil, zero, fmt.Errorf("memory is not available on this Companion")
	}
	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "recall":
		_, r, err := t.memoryRecall(ctx, req, memoryRecallInput{WorkspaceID: in.WorkspaceID, Limit: in.Limit})
		if err != nil {
			return nil, zero, err
		}
		return nil, memoryOutput{Page: r.Page, UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy, Plan: r.Plan,
			Notes: r.Notes, NoteCount: r.NoteCount, Archived: r.Archived, Hint: r.Hint}, nil

	case "note":
		_, n, err := t.memoryNote(ctx, req, memoryNoteInput{WorkspaceID: in.WorkspaceID,
			Title: in.Title, Body: in.Body, ChangeSetID: in.ChangeSetID, RunID: in.RunID, Page: in.Page})
		if err != nil {
			return nil, zero, err
		}
		return nil, memoryOutput{NoteID: n.NoteID, PageUpdated: n.PageUpdated, Cut: n.Cut, NoteCount: n.Notes}, nil

	case "search":
		_, s, err := t.memorySearch(ctx, req, memorySearchInput{WorkspaceID: in.WorkspaceID, Query: in.Query, Limit: in.Limit})
		if err != nil {
			return nil, zero, err
		}
		return nil, memoryOutput{Matches: s.Matches}, nil

	case "read":
		_, r, err := t.memoryRead(ctx, req, memoryReadInput{WorkspaceID: in.WorkspaceID,
			IDs: in.IDs, BeforeID: in.BeforeID, Limit: in.Limit})
		if err != nil {
			return nil, zero, err
		}
		return nil, memoryOutput{FullNotes: r.Notes, NextBeforeID: r.NextBeforeID}, nil

	case "plan":
		plan, cut, err := t.planSteps(ctx, in.WorkspaceID, in.Steps)
		if err != nil {
			return nil, zero, err
		}
		return nil, memoryOutput{Plan: plan, Cut: cut, Hint: planHint(plan)}, nil

	case "step":
		plan, cut, err := t.planStep(ctx, in.WorkspaceID, in.StepID, in.State, in.Note, in.Note != "")
		if err != nil {
			return nil, zero, err
		}
		return nil, memoryOutput{Plan: plan, Cut: cut, Hint: planHint(plan)}, nil

	case "compact":
		_, c, err := t.memoryCompact(ctx, req, memoryCompactInput{WorkspaceID: in.WorkspaceID,
			Summary: in.Summary, ThroughID: in.ThroughID})
		if err != nil {
			return nil, zero, err
		}
		return nil, memoryOutput{ToSummarize: c.Notes, ThroughID: c.ThroughID, ArchivedNow: c.Archived,
			SummaryID: c.SummaryID, Live: c.Live, Hint: c.Hint}, nil

	case "":
		return nil, zero, fmt.Errorf("action is required: recall, note, plan, step, search, read, or compact")
	default:
		return nil, zero, fmt.Errorf("unknown action %q; use recall, note, plan, step, search, read, or compact", in.Action)
	}
}

// memoryDescription is built rather than written out because compaction
// depends on the store: a branch this Companion cannot perform should not
// be advertised, or the model spends a round trip learning that.
func memoryDescription(compacts bool) string {
	var b strings.Builder
	b.WriteString("What this workspace remembers between conversations, selected by action. Nothing here is written into the project's files. ")
	b.WriteString("action=recall returns the current-state page (goal, progress, next, decisions, open questions) and the newest note titles — call it at the start of work on a workspace that has memory; workspace_info says whether it does. ")
	b.WriteString("action=note remembers something for the next conversation: a note (title, body; optionally the change_set_id or run_id it is about) and/or a rewrite of the page. Write a note when a task is finished, a decision is taken, or something was learned the hard way; rewrite the page when the plan changes. ")
	b.WriteString("action=plan writes the steps this workspace is working through, in order, replacing the previous plan — write one when work starts or the approach changes. ")
	b.WriteString("action=step moves one step by its id to todo, doing, blocked or done, with an optional note saying why; both answer with the whole plan, so nothing else has to be called to see where the work stands. Mark a step doing before starting it: a conversation that is cut off mid-turn leaves that mark, and it is how the next one knows where to continue. ")
	b.WriteString("action=search finds notes by words in their title or body. ")
	b.WriteString("action=read returns notes in full: by ids, or paged newest first with before_id. ")
	if compacts {
		b.WriteString("action=compact folds the oldest notes into one summary when recall says the trail is long: call it with nothing else to receive the oldest notes and a through_id, then call it again with summary and through_id; the notes it covers are archived, not deleted. ")
	}
	b.WriteString("Fields have byte limits and are cut to them; the answer names what was cut. ")
	b.WriteString(`Examples: {"action":"recall"} · {"action":"note","title":"the refresh loop is fixed","body":"the token was rotated twice per call; one call site now owns it"} · {"action":"note","page":{"goal":"ship the billing page","next":"wire the retry"}} · {"action":"plan","steps":[{"title":"read the auth middleware"},{"title":"move the refresh to one call site","state":"doing"},{"title":"add a regression test"}]} · {"action":"step","step_id":2,"state":"done"} · {"action":"step","step_id":3,"state":"blocked","note":"the fixture server has no refresh endpoint"} · {"action":"search","query":"refresh token"} · {"action":"read","ids":[12,13]} · {"action":"read","before_id":40,"limit":5}`)
	if compacts {
		b.WriteString(` · {"action":"compact"}`)
	}
	b.WriteString(".")
	return b.String()
}

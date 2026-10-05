package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/store"
)

// The plan is the half of a harness a web chat does not have. A turn that
// carries tool calls is cut off after about 25 minutes, and the next
// conversation starts knowing nothing; the page in memory says where the
// work stands in prose, and this says it in steps, with one of them
// marked as the one in flight. That is what makes "continue" answerable.
//
// Writing the plan replaces it whole; moving a step touches one row. The
// asymmetry is deliberate: a step moves often and must not cost the whole
// plan, in tokens or in a lost update from a conversation on another
// platform.
//
// Bounds are applied here, on the way in, the same way notes are: what is
// too long is cut and the answer names what was cut, so length is dealt
// with where text arrives rather than by compressing later.

// planSteps writes a whole plan and answers with it as stored.
func (t *toolset) planSteps(ctx context.Context, workspaceID string, in []planStepInput) ([]*store.MemoryStep, []string, error) {
	if len(in) == 0 {
		return nil, nil, fmt.Errorf("a plan needs at least one step; to drop the plan, write one step saying what is left")
	}
	ws, err := t.open(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	var cut []string
	if len(in) > store.MemoryPlanSteps {
		in = in[:store.MemoryPlanSteps]
		cut = append(cut, "steps")
	}
	steps := make([]store.MemoryStep, 0, len(in))
	titleCut, noteCut := false, false
	for _, s := range in {
		title, was := bound(strings.TrimSpace(s.Title), store.MemoryStepTitleBytes)
		titleCut = titleCut || was
		note, was := bound(strings.TrimSpace(s.Note), store.MemoryStepNoteBytes)
		noteCut = noteCut || was
		state := strings.ToLower(strings.TrimSpace(s.State))
		if state == "" {
			state = store.StepTodo
		}
		if !store.ValidStepState(state) {
			return nil, nil, fmt.Errorf("state %q is not one of todo, doing, blocked, done", s.State)
		}
		steps = append(steps, store.MemoryStep{Title: title, State: state, Note: note})
	}
	if titleCut {
		cut = append(cut, "step titles")
	}
	if noteCut {
		cut = append(cut, "step notes")
	}
	saved, err := t.memory.SaveMemoryPlan(ctx, ws.ID(), t.provider, steps, time.Time{})
	if err != nil {
		return nil, nil, err
	}
	return saved, cut, nil
}

// planStep moves one step and answers with the whole plan. Returning the
// plan rather than the one row is the point: the model's next question is
// always "what now", and a web chat pays for every round trip twice, in
// message quota and against the turn's clock.
func (t *toolset) planStep(ctx context.Context, workspaceID string, id int64, state, note string, setNote bool) ([]*store.MemoryStep, []string, error) {
	if id <= 0 {
		return nil, nil, fmt.Errorf("step_id is required: take it from the plan in action=recall")
	}
	ws, err := t.open(ctx, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	state = strings.ToLower(strings.TrimSpace(state))
	if !store.ValidStepState(state) {
		return nil, nil, fmt.Errorf("state %q is not one of todo, doing, blocked, done", state)
	}
	var (
		cut     []string
		bounded *string
	)
	if setNote {
		text, was := bound(strings.TrimSpace(note), store.MemoryStepNoteBytes)
		if was {
			cut = append(cut, "note")
		}
		bounded = &text
	}
	// The tool moves a step; it never retitles one. Retitling is the
	// desktop's (D40), and a model that wants different words rewrites the
	// plan whole.
	err = t.memory.UpdateMemoryStep(ctx, ws.ID(), t.provider, id, state, nil, bounded, time.Time{})
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("this plan has no step %d; the plan may have been rewritten since — read it with action=recall", id)
	}
	if err != nil {
		return nil, nil, err
	}
	plan, err := t.memory.ListMemoryPlan(ctx, ws.ID())
	if err != nil {
		return nil, nil, err
	}
	return plan, cut, nil
}

// planHint is the one line a model needs to act without reading the whole
// plan again: how far it has got, and which step is the live one.
func planHint(steps []*store.MemoryStep) string {
	if len(steps) == 0 {
		return ""
	}
	done, blocked := 0, 0
	var live *store.MemoryStep
	for _, s := range steps {
		switch s.State {
		case store.StepDone:
			done++
		case store.StepBlocked:
			blocked++
		}
		if live == nil && s.State == store.StepDoing {
			live = s
		}
	}
	if live == nil {
		for _, s := range steps {
			if s.State == store.StepTodo {
				live = s
				break
			}
		}
	}
	hint := fmt.Sprintf("Plan: %d of %d done", done, len(steps))
	if blocked > 0 {
		hint += fmt.Sprintf(", %d blocked", blocked)
	}
	switch {
	case live != nil && live.State == store.StepDoing:
		hint += fmt.Sprintf(". In flight: #%d %s", live.ID, live.Title)
	case live != nil:
		hint += fmt.Sprintf(". Next: #%d %s", live.ID, live.Title)
	default:
		hint += ". Nothing is left to do; write a new plan with action=plan when the next piece of work starts."
	}
	if live != nil {
		hint += " — move it with action=step."
	}
	return hint
}

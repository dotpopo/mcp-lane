package ctlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/store"
)

// Changing one step by hand (U-T5 / D40). What the desktop may change is the
// half that leaves the id where it is; everything that would renumber the
// plan stays the AI's.
func TestOneStepOfThePlanCanBeChangedByHand(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ws, err := f.manager.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seed := func() []*store.MemoryStep {
		t.Helper()
		plan, err := f.st.SaveMemoryPlan(ctx, ws.ID, "chatgpt", []store.MemoryStep{
			{Title: "read the connection layer", State: store.StepDone},
			{Title: "delete the poller", State: store.StepDoing},
			{Title: "add the settings toggle", State: store.StepBlocked, Note: "no design board yet"},
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	plan := seed()
	post := func(body map[string]any) (*http.Response, []byte) {
		t.Helper()
		body["workspace_id"] = ws.ID
		return f.call(t, "POST", "/v1/memory/plan/step", f.token, body)
	}
	read := func(raw []byte) []*store.MemoryStep {
		t.Helper()
		var got []*store.MemoryStep
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("answer was not a plan: %s", raw)
		}
		return got
	}

	// A state change touches the state, credits the person, and leaves the
	// words alone. The answer is the whole plan, so the screen never has to
	// ask again for what it just changed.
	resp, raw := post(map[string]any{"id": plan[2].ID, "state": "done"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state change = %d %s", resp.StatusCode, raw)
	}
	got := read(raw)
	if len(got) != 3 {
		t.Fatalf("answer carried %d steps, want the whole plan", len(got))
	}
	if got[2].State != store.StepDone || got[2].Title != "add the settings toggle" {
		t.Fatalf("changed step = %+v", got[2])
	}
	if got[2].Note != "no design board yet" {
		t.Fatalf("a change that never mentioned the note rewrote it: %q", got[2].Note)
	}
	if got[2].Provider != "user" {
		t.Fatalf("provider = %q; a hand change is the user's", got[2].Provider)
	}

	// A title change must carry the step's current state through. Getting
	// this wrong is silent: every retitled step would quietly become todo.
	resp, raw = post(map[string]any{"id": plan[1].ID, "title": "delete internal/poll and run the tests"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("title change = %d %s", resp.StatusCode, raw)
	}
	if got = read(raw); got[1].Title != "delete internal/poll and run the tests" || got[1].State != store.StepDoing {
		t.Fatalf("retitled step = %+v", got[1])
	}

	// Absent and empty are different answers: an empty note clears it.
	if _, raw = post(map[string]any{"id": plan[2].ID, "note": ""}); read(raw)[2].Note != "" {
		t.Fatalf("an empty note did not clear it: %+v", read(raw)[2])
	}

	// The bounds the tool obeys are the Core's, not the tool's. Each of
	// these is a "shorten it" answer, never a 500.
	for name, body := range map[string]map[string]any{
		"no title":      {"id": plan[0].ID, "title": "   "},
		"long title":    {"id": plan[0].ID, "title": strings.Repeat("t", store.MemoryStepTitleBytes+1)},
		"long note":     {"id": plan[0].ID, "note": strings.Repeat("n", store.MemoryStepNoteBytes+1)},
		"invented":      {"id": plan[0].ID, "state": "almost"},
		"nothing given": {"id": plan[0].ID},
	} {
		if resp, raw = post(body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, resp.StatusCode, raw)
		}
	}

	// Rewriting the plan replaces every row, so the id the screen is holding
	// stops resolving. That has to be said, not silently matched to nothing:
	// it is the exact sentence board 23 puts on screen.
	old := plan[0].ID
	seed()
	resp, raw = post(map[string]any{"id": old, "state": "done"})
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(raw), "no such step") {
		t.Fatalf("a stale id = %d %s, want 404 saying the step is gone", resp.StatusCode, raw)
	}
}

func TestMemoryEndpointsReadCorrectExportAndForget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ws, err := f.manager.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Empty: a document with nothing in it, never a 404.
	resp, raw := f.call(t, "GET", "/v1/memory", f.token, nil)
	var doc memoryDoc
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &doc) != nil || doc.State != nil || len(doc.Notes) != 0 || len(doc.Plan) != 0 {
		t.Fatalf("fresh memory = %d %s", resp.StatusCode, raw)
	}

	// The platform writes; the desktop reads it back with counts and paging.
	page := store.MemoryPage{Goal: "ship idle sync", Progress: "layer done", Decisions: []string{"imap idle"}, Open: []string{"icloud heartbeat?"}}
	if err := f.st.SaveMemoryState(ctx, ws.ID, "chatgpt", page, time.Now()); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := 0; i < memoryPageSize+2; i++ {
		n := &store.MemoryNote{WorkspaceID: ws.ID, Provider: "chatgpt", Title: "step " + strings.Repeat("x", i%3) + string(rune('a'+i%26)), Body: "body"}
		if err := f.st.AddMemoryNote(ctx, n); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}
	if _, err := f.st.ArchiveMemoryNotes(ctx, ws.ID, ids[1]); err != nil {
		t.Fatal(err)
	}
	// The plan rides in the same answer as the page and the notes: the
	// screen draws all three together, so asking for them separately would
	// be a round trip for nothing.
	if _, err := f.st.SaveMemoryPlan(ctx, ws.ID, "chatgpt", []store.MemoryStep{
		{Title: "delete the poller", State: store.StepDoing},
		{Title: "update the changelog"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, raw = f.call(t, "GET", "/v1/memory?workspace_id="+ws.ID, f.token, nil)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &doc) != nil {
		t.Fatalf("memory = %d %s", resp.StatusCode, raw)
	}
	if doc.State == nil || doc.State.Page.Goal != "ship idle sync" || doc.State.Provider != "chatgpt" {
		t.Fatalf("state = %+v", doc.State)
	}
	if len(doc.Plan) != 2 || doc.Plan[0].Position != 1 || doc.Plan[0].State != store.StepDoing || doc.Plan[1].State != store.StepTodo {
		t.Fatalf("plan = %+v", doc.Plan)
	}
	if doc.Live != memoryPageSize || doc.Archived != 2 || len(doc.Notes) != memoryPageSize || doc.NextBeforeID != 0 {
		t.Fatalf("first page: live %d archived %d notes %d next %d", doc.Live, doc.Archived, len(doc.Notes), doc.NextBeforeID)
	}
	if doc.Notes[0].ID != ids[len(ids)-1] {
		t.Fatalf("listing is not newest first: %d", doc.Notes[0].ID)
	}
	// Fresh documents per answer: decoding into a reused one would keep an
	// omitted false "archived" from the previous page.
	var past memoryDoc
	resp, raw = f.call(t, "GET", "/v1/memory?workspace_id="+ws.ID+"&archived=1", f.token, nil)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &past) != nil || len(past.Notes) != 2 || !past.Notes[0].Archived {
		t.Fatalf("archived page = %d %s", resp.StatusCode, raw)
	}
	var found memoryDoc
	resp, raw = f.call(t, "GET", "/v1/memory?workspace_id="+ws.ID+"&q=step", f.token, nil)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &found) != nil || len(found.Notes) != memoryPageSize || found.Notes[0].Archived {
		t.Fatalf("search = %d %s", resp.StatusCode, raw)
	}
	resp, raw = f.call(t, "GET", "/v1/memory?workspace_id=ws_nope", f.token, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown workspace = %d %s", resp.StatusCode, raw)
	}

	// The user corrects the page by hand: it is credited to them, and an
	// over-long field is refused rather than cut.
	page.Progress = "layer done, poller still running"
	resp, raw = f.call(t, "POST", "/v1/memory/page", f.token, map[string]any{"workspace_id": ws.ID, "page": page})
	var state store.MemoryState
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &state) != nil || state.Provider != memoryUserProvider || state.Page.Progress != page.Progress {
		t.Fatalf("page save = %d %s", resp.StatusCode, raw)
	}
	long := page
	long.Goal = strings.Repeat("g", store.MemoryGoalBytes+1)
	resp, raw = f.call(t, "POST", "/v1/memory/page", f.token, map[string]any{"workspace_id": ws.ID, "page": long})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "limit") {
		t.Fatalf("over-long page = %d %s", resp.StatusCode, raw)
	}

	// One note goes; a second try says it is gone.
	resp, raw = f.call(t, "POST", "/v1/memory/notes/delete", f.token, map[string]any{"workspace_id": ws.ID, "id": ids[5]})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d %s", resp.StatusCode, raw)
	}
	resp, _ = f.call(t, "POST", "/v1/memory/notes/delete", f.token, map[string]any{"workspace_id": ws.ID, "id": ids[5]})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d", resp.StatusCode)
	}

	// Export carries the page, both halves of the trail, and no absolute path.
	resp, raw = f.call(t, "GET", "/v1/memory/export?workspace_id="+ws.ID, f.token, nil)
	var out memoryExport
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
		t.Fatalf("export = %d %s", resp.StatusCode, raw)
	}
	if !strings.HasSuffix(out.Filename, ".md") || strings.ContainsAny(out.Filename, "/ ") {
		t.Fatalf("filename = %q", out.Filename)
	}
	for _, want := range []string{"# Memory: ", "### Goal\n\nship idle sync", "- imap idle", "### Open questions", "## Plan\n", "- [»] delete the poller", "- [ ] update the changelog", "## Notes\n", "## Archived notes\n", "_", "poller still running"} {
		if !strings.Contains(out.Markdown, want) {
			t.Errorf("export lacks %q:\n%s", want, out.Markdown)
		}
	}
	if strings.Contains(out.Markdown, f.root) {
		t.Fatal("export names the root path")
	}

	// Forgetting is total.
	resp, raw = f.call(t, "POST", "/v1/memory/clear", f.token, map[string]any{"workspace_id": ws.ID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear = %d %s", resp.StatusCode, raw)
	}
	var gone memoryDoc
	resp, raw = f.call(t, "GET", "/v1/memory?workspace_id="+ws.ID+"&archived=1", f.token, nil)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &gone) != nil || gone.State != nil || gone.Live+gone.Archived != 0 || len(gone.Notes) != 0 || len(gone.Plan) != 0 {
		t.Fatalf("after clear = %d %s", resp.StatusCode, raw)
	}
}

func TestExportFilenameIsSafeToSave(t *testing.T) {
	at := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"InboxKit":     "InboxKit-memory-2026-09-12.md",
		"my project/2": "my-project-2-memory-2026-09-12.md",
		`a<b>:"c"|?*`:  "ab-c-memory-2026-09-12.md",
		"   ":          "workspace-memory-2026-09-12.md",
		"..":           "workspace-memory-2026-09-12.md",
		"收件箱 同步":       "收件箱-同步-memory-2026-09-12.md",
	} {
		if got := exportFilename(in, at); got != want {
			t.Errorf("exportFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

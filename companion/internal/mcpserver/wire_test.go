package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dotpopo/mcp-lane/shared/tunnel"
)

// providerStamp names the calling platform on every request, the way the
// relay and the direct surface both do once they have checked the token.
type providerStamp struct{ provider string }

func (s providerStamp) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(tunnel.ProviderHeader, s.provider)
	return http.DefaultTransport.RoundTrip(r)
}

// startProviderSession connects as a named platform, so the server serving
// the session is the one that platform's callers get.
func startProviderSession(t *testing.T, provider string) (*mcp.ClientSession, string) {
	t.Helper()
	root := t.TempDir()
	httpServer := httptest.NewServer(Handler(testDeps(t, root), nil))
	t.Cleanup(httpServer.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "fylane-test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   httpServer.URL,
		HTTPClient: &http.Client{Transport: providerStamp{provider: provider}},
	}, nil)
	if err != nil {
		t.Fatalf("client.Connect (handshake): %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, root
}

// Claude is handed only the content blocks, so the structured half is freight
// it cannot read. It must still get the body — the point is to send one copy,
// not to send less.
func TestClaudeGetsOneCopyOfTheAnswer(t *testing.T) {
	session, root := startProviderSession(t, "claude")
	writeTree(t, root, map[string]string{"a.txt": "alpha\n"})

	res := callTool(t, session, "read_file", map[string]any{"path": "a.txt"})
	if res.StructuredContent != nil {
		t.Fatalf("the half Claude never reads was sent anyway: %v", res.StructuredContent)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content blocks = %+v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "alpha") {
		t.Fatalf("the body Claude does read is missing: %+v", res.Content[0])
	}
}

// ChatGPT and Grok read both halves, so the generated duplicate is what goes.
// Keeping the structured one leaves the tool's output schema honoured.
func TestChatGPTAndGrokGetTheStructuredHalfOnly(t *testing.T) {
	for _, provider := range []string{"chatgpt", "grok"} {
		t.Run(provider, func(t *testing.T) {
			session, root := startProviderSession(t, provider)
			writeTree(t, root, map[string]string{"a.txt": "alpha\n"})

			res := callTool(t, session, "read_file", map[string]any{"path": "a.txt"})
			if len(res.Content) != 0 {
				t.Fatalf("the duplicate body was sent anyway: %+v", res.Content)
			}
			var out readFilesOutput
			structured(t, res, &out)
			if len(out.Files) != 1 || out.Files[0].Content != "alpha\n" {
				t.Fatalf("the half these platforms read is wrong: %+v", out)
			}
		})
	}
}

// A caller nobody measured keeps the spec-complete shape. This is the gate
// itself: without it the trim would apply to local clients that read
// structured output strictly.
func TestACallerWeDoNotKnowKeepsBothHalves(t *testing.T) {
	session, root := startSession(t)
	writeTree(t, root, map[string]string{"a.txt": "alpha\n"})

	res := callTool(t, session, "read_file", map[string]any{"path": "a.txt"})
	if res.StructuredContent == nil || len(res.Content) == 0 {
		t.Fatalf("an unrecognised caller must keep both halves: %+v", res)
	}
}

// Text a tool wrote itself is not a duplicate of anything. page_snapshot's
// dimensions and image_probe's instruction exist precisely because the
// structured half does not reach Claude; deleting them as copies would take
// away the only sentence explaining the answer.
func TestAHandWrittenBlockIsNeverMistakenForTheCopy(t *testing.T) {
	written := "Snapshot of http://localhost:5173: 1280x800 JPEG, 84 KiB."
	res := &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: written}},
		StructuredContent: json.RawMessage(`{"status":"ok"}`),
	}

	trimCallResult(res, dropContentCopy)
	if len(res.Content) != 1 {
		t.Fatal("a sentence the structured half does not contain was dropped as a duplicate")
	}

	// Under Claude's rule the same block survives and the structured half goes.
	trimCallResult(res, dropStructured)
	if len(res.Content) != 1 || res.StructuredContent != nil {
		t.Fatalf("result = %+v", res)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != written {
		t.Fatalf("text = %q", got)
	}
}

// A call that failed says why in its content blocks and has no structured
// half at all. Trimming that would replace a reason with nothing.
func TestAFailedCallKeepsItsMessage(t *testing.T) {
	for _, trim := range []wireTrim{dropStructured, dropContentCopy, keepBothCopies} {
		res := &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "file not found: a.txt"}},
		}
		trimCallResult(res, trim)
		if len(res.Content) != 1 {
			t.Fatalf("trim %d took away the only thing the model could act on: %+v", trim, res)
		}
	}
}

// A tool answering through the structured half alone keeps it even on Claude.
// Sending nothing is never the better half.
func TestAnAnswerWithNoContentBlocksIsNeverEmptied(t *testing.T) {
	res := &mcp.CallToolResult{StructuredContent: json.RawMessage(`{"status":"ok"}`)}
	trimCallResult(res, dropStructured)
	if res.StructuredContent == nil {
		t.Fatal("the only copy of the answer was dropped")
	}
}

package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotpopo/mcp-lane/companion/internal/txn"
)

// The shapes we claim to read. Each of these is what a real toolchain prints;
// a shape that stops being recognised costs the model a round trip, which is
// the whole thing U-T3 is buying back.
func TestScanFailureReadsTheShapesWeClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		out    string
		path   string
		line   int
		column int
	}{
		{"go", "./internal/app/run.go:42:6: undefined: helper", "./internal/app/run.go", 42, 6},
		{"go test", "    run_test.go:18: want 3, got 4", "run_test.go", 18, 0},
		{"tsc", "src/app.ts(12,5): error TS2322: Type 'string'...", "src/app.ts", 12, 5},
		{"python", `  File "app/main.py", line 7, in handler`, "app/main.py", 7, 0},
		{"node", "    at load (/srv/app/lib/load.js:9:15)", "/srv/app/lib/load.js", 9, 15},
		{"rustc", "error[E0425]: cannot find value\n --> src/main.rs:4:13", "src/main.rs", 4, 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sites := scanFailure("", tc.out)
			if len(sites) == 0 {
				t.Fatalf("no location found in %q", tc.out)
			}
			got := sites[0]
			if got.path != tc.path || got.line != tc.line || got.column != tc.column {
				t.Fatalf("got %+v, want %s:%d:%d", got, tc.path, tc.line, tc.column)
			}
		})
	}
}

// The summary is the lines that carry the failure, not the whole log: a test
// run prints hundreds of lines and two of them say what broke.
func TestScanFailureKeepsTheLinesThatSayWhatBroke(t *testing.T) {
	stdout := strings.Join([]string{
		"ok  \tgithub.com/x/quiet\t0.02s",
		"=== RUN   TestThing",
		"    thing_test.go:18: want 3, got 4",
		"--- FAIL: TestThing (0.00s)",
		"FAIL\tgithub.com/x/thing\t0.10s",
	}, "\n")
	summary, sites := scanFailure(stdout, "")
	if strings.Contains(summary, "quiet") {
		t.Fatalf("the summary kept a passing package: %q", summary)
	}
	for _, want := range []string{"thing_test.go:18", "--- FAIL: TestThing"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q lacks %q", summary, want)
		}
	}
	if len(sites) != 1 || sites[0].line != 18 {
		t.Fatalf("sites = %+v", sites)
	}

	// Output that announces nothing still answers with its tail rather than
	// with silence: the end is where a build says why it stopped.
	if summary, _ := scanFailure("", "something went sideways\nand then stopped"); !strings.Contains(summary, "stopped") {
		t.Fatalf("unannounced failure = %q", summary)
	}
}

// The security shape of this feature. A failing command prints paths, and
// those paths are attacker-influenced: a test fixture, a dependency, or the
// model itself can put any string in them. Nothing outside the workspace and
// nothing the workspace hides may be read back, and none of it may raise a
// prompt — the user never asked to open these files.
func TestAFailureNeverOpensWhatTheWorkspaceHides(t *testing.T) {
	f := newExecFixture(t, txn.Decision{Approved: true})
	ctx := context.Background()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(f.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/app/run.go", "package app\n\nfunc run() {\n\thelper()\n}\n")
	write(".env", "SECRET=1\n")
	write("node_modules/pkg/index.js", "module.exports = 1\n")

	ws, err := f.tools.open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	stderr := strings.Join([]string{
		"./internal/app/run.go:4:2: undefined: helper",
		"/etc/passwd:1:1: nope",
		"../outside/secrets.go:2:1: nope",
		".env:1:1: nope",
		"node_modules/pkg/index.js:1:1: nope",
	}, "\n")

	out := f.tools.observeFailure(ctx, ws, "", stderr)
	if out == nil {
		t.Fatal("a failure with a real location answered with nothing")
	}
	if len(out.Sites) != 1 {
		t.Fatalf("sites = %+v; only the workspace file may come back", out.Sites)
	}
	site := out.Sites[0]
	if site.Path != "internal/app/run.go" || site.Line != 4 || site.Column != 2 {
		t.Fatalf("site = %+v", site)
	}
	if site.Message != "undefined: helper" {
		t.Fatalf("message = %q", site.Message)
	}
	// The code is already here: that is the round trip this saves.
	if !strings.Contains(site.Excerpt, "helper()") || site.StartLine != 1 {
		t.Fatalf("excerpt = %q from line %d", site.Excerpt, site.StartLine)
	}
	// Nothing hidden leaked, in any field.
	whole := site.Path + site.Message + site.Excerpt + out.Summary
	for _, forbidden := range []string{"SECRET", "module.exports", "root:"} {
		if strings.Contains(whole, forbidden) {
			t.Fatalf("a hidden file's content reached the answer: %q", whole)
		}
	}
	// Reading those files must not have put a question on the user's screen.
	if n := len(f.approver.requests()); n != 0 {
		t.Fatalf("looking at a failure raised %d approvals", n)
	}
}

// End to end: a command that fails comes back with the failure in the same
// answer, and one that succeeds carries none.
func TestAFailedCommandBringsBackWhatItPrinted(t *testing.T) {
	skipOnWindows(t)
	f := newExecFixture(t, txn.Decision{Approved: true})

	out := f.run(t, runCommandInput{Command: []string{"ls", "no-such-directory"}})
	if out.ExitCode == nil || *out.ExitCode == 0 {
		t.Fatalf("expected a failing command: %+v", out)
	}
	if out.Failure == nil || out.Failure.Summary == "" {
		t.Fatalf("a failed command said nothing about why: %+v", out.Failure)
	}

	if ok := f.run(t, runCommandInput{Command: []string{"echo", "fine"}}); ok.Failure != nil {
		t.Fatalf("a command that worked carried a failure: %+v", ok.Failure)
	}
}

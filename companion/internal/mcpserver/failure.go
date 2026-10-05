package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dotpopo/mcp-lane/companion/internal/sandbox"
	"github.com/dotpopo/mcp-lane/companion/internal/textenc"
	"github.com/dotpopo/mcp-lane/companion/internal/workspace"
)

// What a failed command was about, in the same response that reports the
// failure.
//
// Without this a model gets a wall of output, has to guess which line
// matters, and then spends a round trip on read_file to see the code. On a
// web chat that round trip is not free: every tool call burns message quota
// and ticks the turn's clock, which is cut off after about 25 minutes. So
// the run and the look at what broke belong in one call.
//
// Nothing here can read anything a caller could not already read with
// read_file, and it reads less: a path the output names is resolved through
// the same sandbox, and an excluded or sensitive file is dropped in silence
// rather than excerpted — and never raises a confirmation prompt, because
// the user did not ask to open that file, a compiler mentioned it.

const (
	// maxFailureSites bounds how many places are brought back. Failures
	// cascade — one bad type can name forty lines — and the first few are
	// where the work is.
	maxFailureSites = 5
	// failureContext is how many lines either side of the named line come
	// with it.
	failureContext = 3
	// maxFailureSummaryLines bounds the lines of output kept as the summary.
	maxFailureSummaryLines = 8
	maxFailureLineBytes    = 200
	maxExcerptBytes        = 800
	// maxExcerptFileBytes skips excerpting a file too large to be source.
	maxExcerptFileBytes = 2 << 20
)

// commandFailure is what a non-zero exit was about.
type commandFailure struct {
	Summary string        `json:"summary,omitempty" jsonschema:"The lines of output that carry the failure, bounded."`
	Sites   []failureSite `json:"sites,omitempty" jsonschema:"Places in this workspace the output named, with the code around them. Already read: do not call read_file for these."`
	More    int           `json:"more_sites,omitempty" jsonschema:"How many further places the output named that are not listed."`
}

// failureSite is one place the output pointed at.
type failureSite struct {
	Path      string `json:"path" jsonschema:"Workspace-relative path."`
	Line      int    `json:"line"`
	Column    int    `json:"column,omitempty"`
	Message   string `json:"message,omitempty" jsonschema:"What the output said about this place."`
	Excerpt   string `json:"excerpt,omitempty" jsonschema:"The lines around it, starting at excerpt_start_line."`
	StartLine int    `json:"excerpt_start_line,omitempty"`
}

// rawSite is a location as the output wrote it, before the sandbox has had
// anything to say about it.
type rawSite struct {
	path    string
	line    int
	column  int
	message string
}

var (
	// path.ext:line[:col] — Go, gcc, clang, rustc, eslint, node stacks.
	reColonSite = regexp.MustCompile(`([^\s:()\[\]"',]+\.[A-Za-z][A-Za-z0-9]{0,7}):(\d+)(?::(\d+))?`)
	// path.ext(line,col) — tsc, MSVC.
	reParenSite = regexp.MustCompile(`([^\s:()\[\]"',]+\.[A-Za-z][A-Za-z0-9]{0,7})\((\d+),(\d+)\)`)
	// Python tracebacks name the file in words.
	rePythonSite = regexp.MustCompile(`File "([^"]+)", line (\d+)`)
)

// failureMarkers are the line openings that say "this line is the failure"
// even when it names no file: a test name, a panic, a plain error line.
var failureMarkers = []string{
	"--- FAIL", "FAIL", "FAILED", "panic:", "fatal error:",
	"error:", "Error:", "ERROR", "Exception", "Traceback",
	"AssertionError", "SyntaxError", "expected", "want ",
}

// observeFailure looks at what a failed command printed and answers with the
// failure and the code it named. stdout and stderr are the redacted text —
// the same bytes the caller is about to receive — so nothing this returns
// can carry a secret the response itself would not.
func (t *toolset) observeFailure(ctx context.Context, ws *workspace.Workspace, stdout, stderr string) *commandFailure {
	summary, raw := scanFailure(stdout, stderr)
	if summary == "" && len(raw) == 0 {
		return nil
	}
	out := &commandFailure{Summary: summary}
	seen := make(map[string]bool, len(raw))
	for _, r := range raw {
		site, ok := t.resolveSite(ctx, ws, r)
		if !ok {
			continue
		}
		key := site.Path + ":" + strconv.Itoa(site.Line)
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(out.Sites) >= maxFailureSites {
			out.More++
			continue
		}
		out.Sites = append(out.Sites, site)
	}
	if out.Summary == "" && len(out.Sites) == 0 {
		return nil
	}
	return out
}

// resolveSite turns a path the output printed into a place in this workspace,
// or refuses. Everything the sandbox refuses, everything the workspace hides,
// and everything outside the workspace is dropped without a word: a failing
// command must not become a way to read somewhere else, and must not put a
// confirmation prompt on the user's screen for a file they never named.
func (t *toolset) resolveSite(ctx context.Context, ws *workspace.Workspace, r rawSite) (failureSite, bool) {
	rel := strings.TrimSpace(r.path)
	if rel == "" || r.line <= 0 {
		return failureSite{}, false
	}
	if filepath.IsAbs(rel) {
		inside, err := filepath.Rel(ws.Root(), filepath.Clean(rel))
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return failureSite{}, false
		}
		rel = inside
	}
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	abs, canonical, err := ws.Resolve(rel, sandbox.OpRead)
	if err != nil {
		return failureSite{}, false
	}
	if ws.Excluded(canonical, false) || ws.Sensitive(canonical) {
		return failureSite{}, false
	}
	site := failureSite{Path: canonical, Line: r.line, Column: r.column, Message: r.message}
	if excerpt, start, ok := excerptAround(abs, r.line); ok {
		site.Excerpt, site.StartLine = excerpt, start
	}
	return site, true
}

// excerptAround reads the lines around line from a text file. A file that is
// not text, is too large to be source, or cannot be read yields no excerpt —
// the location itself is still worth returning.
func excerptAround(abs string, line int) (string, int, bool) {
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxExcerptFileBytes {
		return "", 0, false
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", 0, false
	}
	text, _, ok := textenc.DetectDecode(data)
	if !ok {
		return "", 0, false
	}
	lines := strings.Split(text, "\n")
	if line > len(lines) {
		return "", 0, false
	}
	start := max(line-failureContext, 1)
	end := min(line+failureContext, len(lines))
	var b strings.Builder
	for i := start; i <= end; i++ {
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\t")
		b.WriteString(strings.TrimRight(lines[i-1], "\r"))
		b.WriteString("\n")
		if b.Len() >= maxExcerptBytes {
			break
		}
	}
	return truncateUTF8(b.String(), maxExcerptBytes), start, true
}

// scanFailure reads the output and answers with the lines that carry the
// failure and the places they named. It is pure text: no path here has been
// checked against anything yet.
//
// stderr is scanned first because it is the channel a compiler uses, and
// stdout second because it is the channel a test runner uses; whichever
// spoke, its lines come back in the order they were printed.
func scanFailure(stdout, stderr string) (string, []rawSite) {
	var (
		kept  []string
		sites []rawSite
	)
	for _, stream := range []string{stderr, stdout} {
		for _, line := range strings.Split(stream, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			found := sitesIn(line)
			sites = append(sites, found...)
			if len(kept) < maxFailureSummaryLines && (len(found) > 0 || marked(line)) {
				kept = append(kept, truncateUTF8(strings.TrimSpace(line), maxFailureLineBytes))
			}
		}
	}
	if len(kept) == 0 {
		// Nothing announced itself. The end of the output is where a build
		// or a run usually says why it stopped, so that is what comes back
		// rather than nothing at all.
		kept = lastLines(stderr, stdout, maxFailureSummaryLines/2)
	}
	return strings.Join(kept, "\n"), sites
}

// sitesIn pulls every location out of one line, in the order they appear.
func sitesIn(line string) []rawSite {
	var out []rawSite
	for _, m := range rePythonSite.FindAllStringSubmatch(line, -1) {
		if n, err := strconv.Atoi(m[2]); err == nil {
			out = append(out, rawSite{path: m[1], line: n, message: pythonMessage(line)})
		}
	}
	for _, m := range reParenSite.FindAllStringSubmatch(line, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		col, _ := strconv.Atoi(m[3])
		out = append(out, rawSite{path: m[1], line: n, column: col, message: after(line, m[0])})
	}
	if len(out) == 0 {
		for _, m := range reColonSite.FindAllStringSubmatch(line, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			col := 0
			if m[3] != "" {
				col, _ = strconv.Atoi(m[3])
			}
			out = append(out, rawSite{path: m[1], line: n, column: col, message: after(line, m[0])})
		}
	}
	return out
}

// after is what the line said about a location: the text following it, minus
// the punctuation a compiler puts in between.
func after(line, match string) string {
	_, rest, found := strings.Cut(line, match)
	if !found {
		return ""
	}
	rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":"))
	return truncateUTF8(rest, maxFailureLineBytes)
}

// pythonMessage keeps the "in <function>" tail of a traceback frame, which is
// the only thing that line says beyond the location.
func pythonMessage(line string) string {
	if _, rest, found := strings.Cut(line, ", in "); found {
		return truncateUTF8("in "+strings.TrimSpace(rest), maxFailureLineBytes)
	}
	return ""
}

func marked(line string) bool {
	trimmed := strings.TrimSpace(line)
	for _, m := range failureMarkers {
		if strings.HasPrefix(trimmed, m) || strings.Contains(trimmed, " "+m) {
			return true
		}
	}
	return false
}

// lastLines is the tail of whichever stream said something.
func lastLines(stderr, stdout string, n int) []string {
	stream := stderr
	if strings.TrimSpace(stream) == "" {
		stream = stdout
	}
	var kept []string
	lines := strings.Split(stream, "\n")
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		kept = append([]string{truncateUTF8(line, maxFailureLineBytes)}, kept...)
	}
	return kept
}

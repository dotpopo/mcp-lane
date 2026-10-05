package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"

	"github.com/dotpopo/mcp-lane/companion/internal/textenc"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func entryPaths(out listDirectoryOutput) []string {
	var paths []string
	for _, e := range out.Entries {
		paths = append(paths, e.Path)
	}
	return paths
}

func TestListDirectory(t *testing.T) {
	session, root := startSession(t)
	writeTree(t, root, map[string]string{
		"readme.md":           "hi\n",
		"src/main.go":         "package main\n",
		"src/util/helper.go":  "package util\n",
		"node_modules/x/i.js": "x\n",           // excluded
		".env":                "S=1\n",         // sensitive
		"vendor/dep/dep.go":   "package dep\n", // excluded
	})

	// Depth 1 on the root: immediate children only, rules applied.
	var out listDirectoryOutput
	structured(t, callTool(t, session, "list_directory", map[string]any{}), &out)
	got := strings.Join(entryPaths(out), ",")
	if got != "readme.md,src" && got != "src,readme.md" {
		t.Fatalf("root depth-1 entries = %v", entryPaths(out))
	}

	// Depth 3: full tree, still no excluded/sensitive paths.
	structured(t, callTool(t, session, "list_directory", map[string]any{"depth": 3}), &out)
	joined := strings.Join(entryPaths(out), ",")
	for _, want := range []string{"src/main.go", "src/util", "src/util/helper.go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("depth-3 listing missing %s: %v", want, entryPaths(out))
		}
	}
	for _, banned := range []string{"node_modules", ".env", "vendor"} {
		if strings.Contains(joined, banned) {
			t.Errorf("listing leaks %s: %v", banned, entryPaths(out))
		}
	}

	// Subdirectory listing.
	structured(t, callTool(t, session, "list_directory", map[string]any{"path": "src", "depth": 1}), &out)
	joined = strings.Join(entryPaths(out), ",")
	if !strings.Contains(joined, "src/main.go") || !strings.Contains(joined, "src/util") {
		t.Errorf("src listing = %v", entryPaths(out))
	}

	// Excluded directory behaves as nonexistent; hostile paths rejected.
	if res := callTool(t, session, "list_directory", map[string]any{"path": "node_modules"}); !res.IsError {
		t.Error("listing an excluded directory must fail as not found")
	}
	if res := callTool(t, session, "list_directory", map[string]any{"path": "../"}); !res.IsError {
		t.Error("listing outside the sandbox must fail")
	}
	// A path that names a file is described rather than refused — U-T1
	// folded stat_path in here. The hash it carries is asserted in
	// workspace_tools_test.go.
	var one listDirectoryOutput
	structured(t, callTool(t, session, "list_directory", map[string]any{"path": "readme.md"}), &one)
	if len(one.Entries) != 1 || one.Entries[0].Type != "file" {
		t.Errorf("describing a file = %+v", one)
	}
	if res := callTool(t, session, "list_directory", map[string]any{"depth": 99}); !res.IsError {
		t.Error("excessive depth must fail")
	}
}

func TestListDirectorySkipsSymlinks(t *testing.T) {
	session, root := startSession(t)
	writeTree(t, root, map[string]string{"real.txt": "x\n"})
	if err := os.Symlink("/etc", filepath.Join(root, "escape")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	var out listDirectoryOutput
	structured(t, callTool(t, session, "list_directory", map[string]any{"depth": 2}), &out)
	if joined := strings.Join(entryPaths(out), ","); strings.Contains(joined, "escape") {
		t.Fatalf("listing followed a symlink: %v", entryPaths(out))
	}
}

func TestReadFiles(t *testing.T) {
	session, root := startSession(t)
	writeTree(t, root, map[string]string{
		"a.txt": "alpha\n",
		"b.txt": "beta\n",
	})

	var out readFilesOutput
	structured(t, callTool(t, session, "read_file", map[string]any{
		"paths": []string{"a.txt", "b.txt", "missing.txt", ".env"},
	}), &out)
	if len(out.Files) != 4 {
		t.Fatalf("files = %d, want 4", len(out.Files))
	}
	if out.Files[0].Content != "alpha\n" || out.Files[0].Error != "" || out.Files[0].Encoding != "utf-8" {
		t.Errorf("a.txt entry = %+v", out.Files[0])
	}
	if out.Files[1].Content != "beta\n" {
		t.Errorf("b.txt entry = %+v", out.Files[1])
	}
	if out.Files[2].Error == "" || out.Files[2].Content != "" {
		t.Errorf("missing.txt entry = %+v, want error", out.Files[2])
	}
	if out.Files[3].Error == "" || !strings.Contains(out.Files[3].Error, "sensitive") {
		t.Errorf(".env entry = %+v, want sensitive denial", out.Files[3])
	}

	if res := callTool(t, session, "read_file", map[string]any{"paths": []string{}}); !res.IsError {
		t.Error("empty batch must fail")
	}
	many := make([]string, maxBatchFiles+1)
	for i := range many {
		many[i] = "a.txt"
	}
	if res := callTool(t, session, "read_file", map[string]any{"paths": many}); !res.IsError {
		t.Error("oversized batch must fail")
	}
}

// U-T1 merged read_files into read_file. One path and many are the same
// read with two different failure modes, and the difference is the point:
// asked for one file, a refusal is the answer and fails the call; asked for
// several, one bad path must not throw away the good reads (covered above).
func TestReadFileTakesOnePathOrMany(t *testing.T) {
	session, root := startSession(t)
	writeTree(t, root, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n"})

	if one := readEntry(t, session, map[string]any{"path": "a.txt"}); one.Content != "alpha\n" || one.Error != "" {
		t.Fatalf("single read = %+v", one)
	}
	var many readFilesOutput
	structured(t, callTool(t, session, "read_file", map[string]any{"paths": []string{"a.txt", "b.txt"}}), &many)
	if len(many.Files) != 2 || many.Files[1].Content != "beta\n" {
		t.Fatalf("batch read = %+v", many.Files)
	}

	// A sandbox refusal on a single path must stay an error result rather
	// than becoming a field the model can read past.
	for _, p := range []string{"missing.txt", "../outside", ".env"} {
		if res := callTool(t, session, "read_file", map[string]any{"path": p}); !res.IsError {
			t.Errorf("read_file(path=%q) must fail the call", p)
		}
	}

	// Ambiguous or impossible inputs are refused, not guessed at.
	for _, args := range []map[string]any{
		{"path": "a.txt", "paths": []string{"b.txt"}},
		{"paths": []string{"a.txt"}, "start_line": 2},
		{},
	} {
		if res := callTool(t, session, "read_file", args); !res.IsError {
			t.Errorf("read_file(%v) must be refused", args)
		}
	}
}

func TestReadFileEncodings(t *testing.T) {
	session, root := startSession(t)

	utf16Content := "hello 世界\n"
	utf16le, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(utf16Content))
	if err != nil {
		t.Fatal(err)
	}
	gbkContent := "中文内容测试\n"
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(gbkContent))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"u16.txt":  utf16le,
		"gbk.txt":  gbk,
		"bin.dat":  {0x89, 0x50, 0x4e, 0x47, 0x00, 0x01, 0x02, 0xff},
		"utf8.txt": []byte("plain\n"),
	} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rd := readEntry(t, session, map[string]any{"path": "u16.txt"})
	if rd.Encoding != "utf-16le" || rd.Content != utf16Content {
		t.Errorf("utf-16 read = %+v", rd)
	}

	rd = readEntry(t, session, map[string]any{"path": "gbk.txt"})
	if rd.Encoding != "gbk" || rd.Content != gbkContent {
		t.Errorf("gbk read = %+v", rd)
	}

	rd = readEntry(t, session, map[string]any{"path": "utf8.txt"})
	if rd.Encoding != "utf-8" || rd.Content != "plain\n" {
		t.Errorf("utf-8 read = %+v", rd)
	}

	// Binary: metadata only, never raw content, and not an error.
	rd = readEntry(t, session, map[string]any{"path": "bin.dat"})
	if rd.Encoding != "binary" || rd.Content != "" || rd.SHA256 == "" || rd.SizeBytes != 8 {
		t.Errorf("binary read = %+v", rd)
	}
}

func TestDetectDecode(t *testing.T) {
	if _, enc, ok := textenc.DetectDecode(nil); !ok || enc != "utf-8" {
		t.Errorf("empty = %v %v", enc, ok)
	}
	if _, _, ok := textenc.DetectDecode([]byte{0x00, 0x01, 0x02}); ok {
		t.Error("NUL-leading binary detected as text")
	}
	// Invalid GBK tail must not be reported as GBK.
	if _, enc, ok := textenc.DetectDecode([]byte{0xd6, 0xd0, 0x81}); ok {
		t.Errorf("truncated GBK reported as text (%s)", enc)
	}
}

func TestAListingHidesWhatTheRepositoryIgnoresAndKeepsWhatItHasNotSeen(t *testing.T) {
	// The whole rule in one assertion pair. The comparison this came from
	// lists a workspace out of the git index, which hides the ignored files
	// and, with them, every file the repository has never seen — the one the
	// user wrote a minute ago included. Here the walk is still a filesystem
	// walk, so the second half keeps showing up; only the first half goes.
	session, root := startSession(t)
	writeTree(t, root, map[string]string{
		".gitignore":       "*.log\nbuild/\n",
		"readme.md":        "hi\n",
		"debug.log":        "noise\n",
		"build/app.bin":    "binary\n",
		"just-written.txt": "the user wrote this and has not committed it\n",
	})

	var out listDirectoryOutput
	structured(t, callTool(t, session, "list_directory", map[string]any{"depth": 3}), &out)
	listed := entryPaths(out)
	joined := strings.Join(listed, ",")

	for _, gone := range []string{"debug.log", "build"} {
		if strings.Contains(joined, gone) {
			t.Errorf("%s is ignored by the repository but was listed: %v", gone, listed)
		}
	}
	for _, kept := range []string{"readme.md", "just-written.txt", ".gitignore"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%s is not ignored and must still be listed: %v", kept, listed)
		}
	}
}

func TestAnIgnoreFileCannotUnhideASensitiveFile(t *testing.T) {
	// A .gitignore is ordinary workspace content, so a model can write one.
	// What it must not be able to do is widen what it can see.
	session, root := startSession(t)
	writeTree(t, root, map[string]string{
		".gitignore":                "!.env\n!node_modules\n",
		".env":                      "AWS_SECRET_ACCESS_KEY=x\n",
		"readme.md":                 "hi\n",
		"node_modules/pkg/index.js": "x\n",
	})

	var out listDirectoryOutput
	structured(t, callTool(t, session, "list_directory", map[string]any{"depth": 3}), &out)
	listed := strings.Join(entryPaths(out), ",")
	if strings.Contains(listed, ".env") {
		t.Errorf("a \"!\" line in a .gitignore revealed a sensitive file: %v", entryPaths(out))
	}
	if strings.Contains(listed, "node_modules") {
		t.Errorf("a \"!\" line in a .gitignore reached the workspace exclude rules: %v", entryPaths(out))
	}
	if !strings.Contains(listed, "readme.md") {
		t.Errorf("the ordinary file went missing too, so the assertions above prove nothing: %v", entryPaths(out))
	}
}

package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dotpopo/mcp-lane/companion/internal/sandbox"
	"github.com/dotpopo/mcp-lane/companion/internal/workspace"
)

const (
	// maxListEntries caps a single list_directory response.
	maxListEntries = 2000
	// maxListDepth caps recursive listing depth.
	maxListDepth = 10
	// maxBatchFiles caps one read_files call.
	maxBatchFiles = 20
	// maxBatchBytes caps the combined content size of a read_files response;
	// beyond it remaining files return an error entry instead of content.
	maxBatchBytes = 4 << 20 // 4 MiB
)

type listDirectoryInput struct {
	WorkspaceID string `json:"workspace_id,omitempty" jsonschema:"Opaque workspace identifier from workspace_info."`
	Path        string `json:"path,omitempty" jsonschema:"Workspace-relative directory path. Omit or use \".\" for the workspace root."`
	Depth       int    `json:"depth,omitempty" jsonschema:"How many directory levels to descend, 1-10. Default 1 (immediate children only)."`
}

type dirEntry struct {
	Path      string `json:"path"`
	Type      string `json:"type" jsonschema:"file or directory"`
	SizeBytes int64  `json:"size_bytes,omitempty" jsonschema:"File size; omitted for directories."`
	// SHA256 is filled only when path named this one file. Hashing every
	// entry of a listing would read the whole tree to answer a question
	// about its shape.
	SHA256 string `json:"sha256,omitempty" jsonschema:"SHA-256 of the file, when path named a single file. This is the expected_sha256 a write wants."`
}

type listDirectoryOutput struct {
	Path      string     `json:"path" jsonschema:"Canonical directory path; empty string is the workspace root."`
	Entries   []dirEntry `json:"entries"`
	Truncated bool       `json:"truncated" jsonschema:"True when the listing was cut off at the entry limit."`
}

func (t *toolset) listDirectory(ctx context.Context, _ *mcp.CallToolRequest, in listDirectoryInput) (*mcp.CallToolResult, listDirectoryOutput, error) {
	var zero listDirectoryOutput
	ws, err := t.open(ctx, in.WorkspaceID)
	if err != nil {
		return nil, zero, err
	}
	depth := in.Depth
	switch {
	case depth == 0:
		depth = 1
	case depth < 0 || depth > maxListDepth:
		return nil, zero, fmt.Errorf("depth must be between 1 and %d", maxListDepth)
	}

	// Listing the root's children is a read on the children, not an
	// operation on the root itself, so "" / "." is allowed here even though
	// the sandbox rejects it for file operations.
	abs, canonical := ws.Root(), ""
	if in.Path != "" && in.Path != "." {
		abs, canonical, err = ws.Resolve(in.Path, sandbox.OpRead)
		if err != nil {
			return nil, zero, err
		}
		if err := t.checkReadable(ctx, ws, canonical); err != nil {
			return nil, zero, err
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, zero, fmt.Errorf("path not found: %s", canonical)
		}
		return nil, zero, fmt.Errorf("listing %s: %w", canonical, err)
	}
	out := listDirectoryOutput{Path: canonical, Entries: []dirEntry{}}
	// A path that names a file is described rather than refused: this is
	// where stat_path went. It answers type, size and the SHA-256 a write
	// needs as expected_sha256, without reading the content.
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil, zero, fmt.Errorf("%s is not a regular file or directory", canonical)
		}
		entry := dirEntry{Path: canonical, Type: "file", SizeBytes: info.Size()}
		if info.Size() <= maxFileBytes {
			data, err := os.ReadFile(abs)
			if err != nil {
				return nil, zero, fmt.Errorf("hashing %s: %w", canonical, err)
			}
			sum := sha256.Sum256(data)
			entry.SHA256 = hex.EncodeToString(sum[:])
		}
		out.Entries = append(out.Entries, entry)
		return nil, out, nil
	}

	if err := listInto(ws, abs, canonical, depth, &out); err != nil {
		return nil, zero, err
	}
	return nil, out, nil
}

// listInto appends the entries of absDir (relative prefix relDir) to out,
// descending depth levels. Excluded and sensitive paths are hidden (PRD
// symlinks are skipped — the sandbox never follows them.
func listInto(ws *workspace.Workspace, absDir, relDir string, depth int, out *listDirectoryOutput) error {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return fmt.Errorf("listing %s: %w", relDir, err)
	}
	for _, e := range entries {
		if out.Truncated {
			return nil
		}
		rel := path.Join(relDir, e.Name())
		if ws.Excluded(rel, e.IsDir()) || ws.Sensitive(rel) {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		switch {
		case e.IsDir():
			if len(out.Entries) >= maxListEntries {
				out.Truncated = true
				return nil
			}
			out.Entries = append(out.Entries, dirEntry{Path: rel, Type: "directory"})
			if depth > 1 {
				if err := listInto(ws, filepath.Join(absDir, e.Name()), rel, depth-1, out); err != nil {
					return err
				}
			}
		case e.Type().IsRegular():
			info, err := e.Info()
			if err != nil {
				continue
			}
			if len(out.Entries) >= maxListEntries {
				out.Truncated = true
				return nil
			}
			out.Entries = append(out.Entries, dirEntry{Path: rel, Type: "file", SizeBytes: info.Size()})
		}
	}
	return nil
}

type readFilesInput struct {
	WorkspaceID string   `json:"workspace_id,omitempty" jsonschema:"Opaque workspace identifier from workspace_info."`
	Path        string   `json:"path,omitempty" jsonschema:"One workspace-relative file path, e.g. src/main.go. Use this or paths, not both."`
	Paths       []string `json:"paths,omitempty" jsonschema:"Several workspace-relative file paths, at most 20 per call. Use this or path, not both."`
	StartLine   int      `json:"start_line,omitempty" jsonschema:"With path: first line to return, 1-based. Omit to read from the beginning."`
	EndLine     int      `json:"end_line,omitempty" jsonschema:"With path: last line to return, 1-based inclusive. Omit to read to the end."`
}

type readFilesEntry struct {
	Path string `json:"path"`
	// Error is set when this file could not be read; the other fields are
	// then empty. One failing file never fails the whole batch.
	Error      string `json:"error,omitempty"`
	Content    string `json:"content,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Encoding   string `json:"encoding,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	TotalLines int    `json:"total_lines,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	// NextStartLine and ResourceURI mirror read_file: set when this entry
	// was truncated, so the rest is reachable via a line-ranged read_file
	// call or resources/read.
	NextStartLine int    `json:"next_start_line,omitempty"`
	ResourceURI   string `json:"resource_uri,omitempty"`
}

type readFilesOutput struct {
	Files []readFilesEntry `json:"files"`
}

func (t *toolset) readFiles(ctx context.Context, _ *mcp.CallToolRequest, in readFilesInput) (*mcp.CallToolResult, readFilesOutput, error) {
	var zero readFilesOutput
	// One path and several are the same read with two different failure
	// modes, which is why they are one tool but not one branch. Asked for
	// one file, a refusal — outside the sandbox, sensitive, absent — is the
	// answer and fails the call. Asked for twenty, one bad path must not
	// throw away nineteen good reads, so it is reported per file.
	paths, single := in.Paths, in.Path != ""
	switch {
	case single && len(in.Paths) > 0:
		return nil, zero, fmt.Errorf("give path for one file or paths for several, not both")
	case single:
		paths = []string{in.Path}
	case len(paths) == 0:
		return nil, zero, fmt.Errorf("give path for one file, or paths for up to %d", maxBatchFiles)
	case in.StartLine > 0 || in.EndLine > 0:
		return nil, zero, fmt.Errorf("a line range applies to one file: use path with start_line and end_line")
	case len(paths) > maxBatchFiles:
		return nil, zero, fmt.Errorf("at most %d paths per call; split the batch", maxBatchFiles)
	}
	ws, err := t.open(ctx, in.WorkspaceID)
	if err != nil {
		return nil, zero, err
	}

	out := readFilesOutput{Files: make([]readFilesEntry, 0, len(paths))}
	total := 0
	for _, p := range paths {
		if total >= maxBatchBytes {
			out.Files = append(out.Files, readFilesEntry{
				Path:        p,
				Error:       "batch content budget exceeded; read this file individually",
				ResourceURI: fileResourceURI(ws.ID(), p),
			})
			continue
		}
		one, err := t.readOne(ctx, ws, p, in.StartLine, in.EndLine)
		if err != nil {
			if single {
				return nil, zero, err
			}
			out.Files = append(out.Files, readFilesEntry{Path: p, Error: err.Error()})
			continue
		}
		total += len(one.Content)
		out.Files = append(out.Files, readFilesEntry{
			Path:          one.Path,
			Content:       one.Content,
			SHA256:        one.SHA256,
			Encoding:      one.Encoding,
			Truncated:     one.Truncated,
			TotalLines:    one.TotalLines,
			SizeBytes:     one.SizeBytes,
			NextStartLine: one.NextStartLine,
			ResourceURI:   one.ResourceURI,
		})
	}
	return nil, out, nil
}

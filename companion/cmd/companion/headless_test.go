package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// init is idempotent: the second run over the same folder verifies and
// re-selects instead of failing, and still exits 0.
func TestInitIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	ws := t.TempDir()

	if err := initCmd([]string{"-data-dir", dataDir, "-workspace", ws, "-non-interactive"}); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if err := initCmd([]string{"-data-dir", dataDir, "-workspace", ws, "-non-interactive"}); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); err != nil {
		t.Fatalf("minimal config missing: %v", err)
	}
}

// init in non-interactive mode fails on a missing folder instead of asking.
func TestInitNonInteractiveRefusesMissingDir(t *testing.T) {
	err := initCmd([]string{"-data-dir", t.TempDir(), "-workspace",
		filepath.Join(t.TempDir(), "no-such-dir"), "-non-interactive"})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing-dir failure, got %v", err)
	}
}

// init requires a workspace, and rejects usage errors with exit code 2.
func TestInitUsageErrors(t *testing.T) {
	if err := initCmd([]string{"-data-dir", t.TempDir()}); exitCodeOf(err) != exitUsage {
		t.Fatalf("missing workspace should be usage (2), got %v", err)
	}
	if err := resolveApproval([]string{}, "approve", true); exitCodeOf(err) != exitUsage {
		t.Fatalf("missing id should be usage (2), got %v", err)
	}
}

// approvals without a running daemon names the cause (no control file),
// not a bare connection refusal.
func TestApprovalsWithoutDaemon(t *testing.T) {
	err := approvalsCmd([]string{"-data-dir", t.TempDir(), "-json"})
	if err == nil || !strings.Contains(err.Error(), "is `serve` running") {
		t.Fatalf("expected no-daemon failure, got %v", err)
	}
}

// resolveDataDir honors the flag, then the environment, then the default.
func TestResolveDataDirPrecedence(t *testing.T) {
	if dir, _ := resolveDataDir("/flag/dir"); dir != "/flag/dir" {
		t.Fatalf("flag should win, got %q", dir)
	}
	t.Setenv("FYLANE_DATA_DIR", "/env/dir")
	if dir, _ := resolveDataDir(""); dir != "/env/dir" {
		t.Fatalf("env should win over default, got %q", dir)
	}
}

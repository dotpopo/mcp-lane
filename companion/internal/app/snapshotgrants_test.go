package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/pagesnap"
)

// TestSnapshotGrantsSurviveInTheSettingsFile: the prompt promises a folder
// will not be asked about again, so the yes has to outlive the process — and
// it must not disturb the other settings living in the same file.
func TestSnapshotGrantsSurviveInTheSettingsFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCommandRung(dir, "workspace"); err != nil {
		t.Fatal(err)
	}
	store := SnapshotGrantStore{DataDir: dir}

	held, err := store.Load()
	if err != nil || len(held) != 0 {
		t.Fatalf("fresh store: %v %v", held, err)
	}

	grants, err := pagesnap.LoadGrants(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := grants.Grant("ws_a"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, settingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"page_snapshot_grants"`) || !strings.Contains(string(raw), "ws_a") {
		t.Fatalf("settings file: %s", raw)
	}
	if rung, err := LoadCommandRung(dir); err != nil || rung != "workspace" {
		t.Fatalf("the rung was lost: %q %v", rung, err)
	}

	restarted, err := pagesnap.LoadGrants(SnapshotGrantStore{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Granted("ws_a") || restarted.Granted("ws_b") {
		t.Fatal("the stored grant did not come back as it was")
	}
	if at := restarted.List()[0].GrantedAt; time.Since(at) > time.Minute || at.IsZero() {
		t.Fatalf("granted at %v", at)
	}

	if had, err := restarted.Revoke("ws_a"); err != nil || !had {
		t.Fatalf("revoke: %v %v", had, err)
	}
	after := SnapshotGrantStore{DataDir: dir}
	if held, err := after.Load(); err != nil || len(held) != 0 {
		t.Fatalf("after revoke: %v %v", held, err)
	}
}

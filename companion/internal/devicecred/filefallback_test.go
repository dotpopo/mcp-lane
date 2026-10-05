package devicecred

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The file fallback is explicit opt-in: without the env var a broken
// keychain stays a loud error that names the fallback.
func TestFileFallbackRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	t.Setenv(CredentialsFileEnv, path)

	relay := "https://relay.example:8443"
	want := Credentials{DeviceID: "dev1", DeviceSecret: "s3cret"}
	if err := saveFile(path, relay, want); err != nil {
		t.Fatalf("saveFile: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || (ownerOnlyEnforced() && fi.Mode().Perm() != 0o600) {
		t.Fatalf("fallback file perms = %v, %v", fi.Mode(), err)
	}
	got, err := loadFile(path, relay)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// Other relays keep their entries.
	if err := saveFile(path, "https://other.example", Credentials{DeviceID: "d", DeviceSecret: "s"}); err != nil {
		t.Fatalf("second saveFile: %v", err)
	}
	if got, err := loadFile(path, relay); err != nil || got != want {
		t.Fatalf("after second save = %+v, %v", got, err)
	}
	if _, err := loadFile(path, "https://unknown.example"); err == nil {
		t.Fatal("unknown relay in fallback file should fail")
	}
}

// A lax pre-existing file is refused, not adopted: the secret would
// otherwise inherit permissions the operator did not choose for it.
// Windows carries no mode bits for the gate to read, so there is nothing
// to refuse by.
func TestFileFallbackRefusesLaxPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix mode bits for the 0600 gate")
	}
	path := filepath.Join(t.TempDir(), "creds.json")
	raw, _ := json.Marshal(map[string]Credentials{})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveFile(path, "https://relay.example", Credentials{DeviceID: "d", DeviceSecret: "s"}); err == nil {
		t.Fatal("lax fallback file should be refused")
	}
}

// File fallback for device credentials on headless machines.
//
// The OS keychain is the only default home for device credentials. On a
// headless Linux server there is often no keychain at all (no D-Bus secret
// service), and silently storing the device secret anywhere else would trade
// a loud failure for a quiet downgrade. So there is no silent fallback:
// the operator opts in explicitly with FYLANE_DEVICE_CREDENTIALS_FILE, and
// every use of it is documented as plaintext-at-rest (see docs/headless.md).
//
// Format: a JSON object mapping the keychain entry name ("relay:<host>")
// to credentials, e.g. {"relay:relay.example:8443": {"device_id": "...",
// "device_secret": "..."}}. The file must be owner-only (0600); wider
// permissions are refused. That gate needs Unix mode bits, so on Windows the
// file instead inherits its directory's ACLs (see ownerOnlyEnforced).
package devicecred

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
)

// ownerOnlyEnforced reports whether an owner-only (0600) file mode can be
// relied on here. Windows carries no Unix mode bits — every file stats as
// 0666 and Go's Chmod there only honors the read-only flag — so the 0600
// gate cannot hold; the fallback file inherits its directory's ACLs.
func ownerOnlyEnforced() bool { return runtime.GOOS != "windows" }

// CredentialsFileEnv names the file fallback for device credentials. It is
// only consulted when the OS keychain cannot be used; the keychain stays
// first whenever it works.
const CredentialsFileEnv = "FYLANE_DEVICE_CREDENTIALS_FILE"

// credentialsFile returns the configured fallback path, or "" when the
// operator did not opt in.
func credentialsFile() string {
	return os.Getenv(CredentialsFileEnv)
}

// loadFile reads one relay's credentials from the fallback file.
func loadFile(path, relayURL string) (Credentials, error) {
	var creds Credentials
	key, err := relayKey(relayURL)
	if err != nil {
		return creds, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return creds, fmt.Errorf("reading %s: %w", CredentialsFileEnv, err)
	}
	var table map[string]Credentials
	if err := json.Unmarshal(raw, &table); err != nil {
		return creds, fmt.Errorf("parsing %s: %w", CredentialsFileEnv, err)
	}
	creds, ok := table[key]
	if !ok || creds.DeviceID == "" || creds.DeviceSecret == "" {
		return creds, fmt.Errorf("no device credentials for this relay in %s; run `fylane-companion pair -relay %s` first",
			CredentialsFileEnv, relayURL)
	}
	return creds, nil
}

// saveFile stores one relay's credentials in the fallback file, preserving
// the entries for other relays. The file is created owner-only (0600); a
// pre-existing file with wider permissions is refused rather than reused.
// Both halves of that gate apply only where modes exist (see
// ownerOnlyEnforced).
func saveFile(path, relayURL string, creds Credentials) error {
	key, err := relayKey(relayURL)
	if err != nil {
		return err
	}
	table := map[string]Credentials{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &table); err != nil {
			return fmt.Errorf("parsing %s: %w", CredentialsFileEnv, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", CredentialsFileEnv, err)
	}
	if fi, err := os.Stat(path); err == nil {
		if ownerOnlyEnforced() && fi.Mode().Perm() != 0o600 {
			return fmt.Errorf("%s must be owner-only (0600), got %o: fix the permissions or move the secret back to the OS keychain",
				CredentialsFileEnv, fi.Mode().Perm())
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stating %s: %w", CredentialsFileEnv, err)
	}
	table[key] = creds
	raw, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", CredentialsFileEnv, err)
	}
	// WriteFile keeps a pre-existing mode; the secret must be owner-only
	// even if something planted a lax file first. Skipped where modes do
	// not exist (see ownerOnlyEnforced).
	if ownerOnlyEnforced() {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("securing %s: %w", CredentialsFileEnv, err)
		}
	}
	return nil
}

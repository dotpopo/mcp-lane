package directsrv

import (
	"testing"

	"github.com/dotpopo/mcp-lane/shared/authsrv"
)

// A client id that resolves to nothing must produce no report at all.
// Guessing a platform here would mark the wrong one as needing to
// reconnect — worse than silence, because the window would then send the
// user to re-authorize a connection nothing had broken.
func TestAnUnresolvableClientIsNotReportedAsAPlatform(t *testing.T) {
	s, _, _ := newTestServer(t)

	var named []string
	s.SetOnRevoked(func(provider string) { named = append(named, provider) })

	if s.auth.OnFamilyRevoked == nil {
		t.Fatal("SetOnRevoked did not install anything on the auth server")
	}
	s.auth.OnFamilyRevoked("client-that-was-never-registered")

	if len(named) != 0 {
		t.Fatalf("named a platform for an unknown client: %v", named)
	}
}

// The happy path: a registered client resolves to the platform the user
// knows it by. The caller keeps its records by provider — a client id is
// reissued on every re-registration and would name nothing twice.
func TestARegisteredClientIsNamedByItsPlatform(t *testing.T) {
	s, _, _ := newTestServer(t)

	if err := s.store.CreateClient(&authsrv.Client{
		ID:           "cl_claude",
		Name:         "Claude",
		RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"},
	}); err != nil {
		t.Fatal(err)
	}

	var named []string
	s.SetOnRevoked(func(provider string) { named = append(named, provider) })
	s.auth.OnFamilyRevoked("cl_claude")

	if len(named) != 1 || named[0] != "claude" {
		t.Fatalf("named %v, want one entry naming claude", named)
	}
}

// A client that registered under a name the product does not recognise is
// not a platform anyone can be told to reconnect. Reporting it would put a
// word on screen that matches nothing the user can act on.
func TestAnUnrecognisedClientIsNotNamedEither(t *testing.T) {
	s, _, _ := newTestServer(t)

	if err := s.store.CreateClient(&authsrv.Client{
		ID:           "cl_someone",
		Name:         "Some MCP Client",
		RedirectURIs: []string{"https://example.invalid/callback"},
	}); err != nil {
		t.Fatal(err)
	}

	var named []string
	s.SetOnRevoked(func(provider string) { named = append(named, provider) })
	s.auth.OnFamilyRevoked("cl_someone")

	if len(named) != 0 {
		t.Fatalf("named a platform for a client we do not know: %v", named)
	}
}

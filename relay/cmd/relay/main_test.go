package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dotpopo/mcp-lane/relay/internal/approverproxy"
	"github.com/dotpopo/mcp-lane/shared/ratelimit"
	"github.com/dotpopo/mcp-lane/shared/tunnel"
)

func legacyFixture(t *testing.T) http.Handler {
	t.Helper()
	ts, err := tunnel.NewServer("shared-secret")
	if err != nil {
		t.Fatal(err)
	}
	return legacyMCP(ts, "shared-secret", ratelimit.New(100, 100))
}

func TestLegacyMCPRequiresToken(t *testing.T) {
	h := legacyFixture(t)

	cases := []struct {
		name   string
		path   string
		bearer string
		want   int
	}{
		// No companion is connected, so an authenticated call reaches the
		// forwarder and gets its 503 — proof the gate passed.
		{"no credentials", "/mcp", "", http.StatusUnauthorized},
		{"wrong bearer", "/mcp", "not-it", http.StatusUnauthorized},
		{"wrong capability segment", "/mcp/not-it", "", http.StatusUnauthorized},
		{"bearer header", "/mcp", "shared-secret", http.StatusServiceUnavailable},
		{"capability url", "/mcp/shared-secret", "", http.StatusServiceUnavailable},
		{"capability url trailing slash", "/mcp/shared-secret/", "", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
		if tc.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
		if rec.Code == http.StatusServiceUnavailable && !strings.Contains(rec.Body.String(), "offline") {
			t.Errorf("%s: authenticated call did not reach the forwarder: %q", tc.name, rec.Body.String())
		}
	}
}

func TestLegacyMCPRateLimited(t *testing.T) {
	ts, err := tunnel.NewServer("shared-secret")
	if err != nil {
		t.Fatal(err)
	}
	h := legacyMCP(ts, "shared-secret", ratelimit.New(1, 2))

	var last int
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/mcp/shared-secret", strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.9:4444"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("third burst call: status %d, want 429", last)
	}
}

func TestWithAccessLogRedactsQuery(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	req := httptest.NewRequest(http.MethodGet, "/authorize?code_challenge=SECRETCHAL&state=SECRETSTATE", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := buf.String()
	if !strings.Contains(line, "GET /authorize -> 400") {
		t.Fatalf("access log line missing or malformed: %q", line)
	}
	if strings.Contains(line, "SECRET") {
		t.Fatalf("query parameters leaked into the log: %q", line)
	}
}

func TestWithIPLimit(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := withIPLimit(ratelimit.New(1, 1), next)

	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/token", nil)
		req.RemoteAddr = "198.51.100.7:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("call %d: status %d, want %d", i, rec.Code, want)
		}
	}

	// A different IP has its own budget.
	req := httptest.NewRequest(http.MethodPost, "/token", nil)
	req.RemoteAddr = "198.51.100.8:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("independent ip throttled: %d", rec.Code)
	}
}

// FYLANE_RELAY_DB decides between PostgreSQL and a SQLite file. Getting this
// wrong on a self-hosted relay means either a startup failure or, worse, a
// file created next to the one holding the real pairings.
func TestIsPostgresDSN(t *testing.T) {
	postgres := []string{
		"postgres://user:pw@db/fylane?sslmode=disable",
		"postgresql://db/fylane",
		"host=127.0.0.1 port=5432 dbname=fylane",
	}
	files := []string{
		"/var/lib/fylane/relay.db",
		"relay.db",
		"C:\\ProgramData\\fylane\\relay.db",
		"",
	}
	for _, dsn := range postgres {
		if !isPostgresDSN(dsn) {
			t.Errorf("isPostgresDSN(%q) = false; want true", dsn)
		}
	}
	for _, dsn := range files {
		if isPostgresDSN(dsn) {
			t.Errorf("isPostgresDSN(%q) = true; want false", dsn)
		}
	}
}

// The rate ceiling is a published number and, until now, three unnamed
// literals. TestWithIPLimit proves the limiter refuses; nothing proved that
// what production hands it is still 5 requests a second with a burst of 20.
func TestThePublishedRateLimitIsTheOneWeShip(t *testing.T) {
	if ratePerSecond != 5 || rateBurst != 20 {
		t.Errorf("rate ceiling = %d/s burst %d, published as 5/s burst 20",
			ratePerSecond, rateBurst)
	}
}

// TestApproverSurfaceIsServedHereAndForwardedFromHere: the page comes from
// the relay, an API call without a routing key stops at the relay, and one
// with a key reaches the forwarder (which, with nobody connected, says so).
func TestApproverSurfaceIsServedHereAndForwardedFromHere(t *testing.T) {
	ts, err := tunnel.NewServer("shared-secret")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mountApprover(mux, ts, approverproxy.ByDeviceID)
	get := func(method, path, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(http.MethodGet, "/approver", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("page: %d, policy %q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
	if rec := get(http.MethodGet, "/approver/manifest.webmanifest", ""); rec.Code != http.StatusOK {
		t.Errorf("manifest: %d", rec.Code)
	}
	if rec := get(http.MethodGet, "/v1/approver/inbox", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("inbox without a credential: %d", rec.Code)
	}
	rec := get(http.MethodGet, "/v1/approver/inbox", "Fylane-Approver dev_a.apr_1:1:nonce0123456789ab:c2ln")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "offline") {
		t.Errorf("inbox with a credential did not reach the forwarder: %d %s", rec.Code, rec.Body)
	}
}

func TestRedactPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/mcp", "/mcp"},
		{"/mcp/", "/mcp/"},
		{"/mcp/shared-secret", "/mcp/REDACTED"},
		{"/mcp/shared-secret/", "/mcp/REDACTED/"},
		{"/mcp/shared-secret/extra", "/mcp/REDACTED/extra"},
		{"/authorize", "/authorize"},
		{"/v1/approver/inbox", "/v1/approver/inbox"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := redactPath(tc.in); got != tc.want {
			t.Errorf("redactPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A legacy capability URL carries the token in the path. The access log must
// keep the endpoint but never the value.
func TestWithAccessLogRedactsCapabilitySegment(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	h := withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp/topsecret-token-value", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := buf.String()
	if !strings.Contains(line, "POST /mcp/REDACTED -> 401") {
		t.Fatalf("access log line missing or unredacted: %q", line)
	}
	if strings.Contains(line, "topsecret") {
		t.Fatalf("capability token leaked into the log: %q", line)
	}
}

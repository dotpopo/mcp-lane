package approverproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dotpopo/mcp-lane/shared/tunnel"
)

// seen is what a Companion behind the relay saw arrive through its tunnel.
type seen struct {
	Device string      `json:"device"`
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Auth   string      `json:"auth"`
	Body   string      `json:"body"`
	Header http.Header `json:"header"`
}

// relayRig is a real relay: a tunnel server whose auth reads the device
// from "<device>:<secret>", the proxy in front of it, and Companions that
// dial in and echo what reached them.
type relayRig struct {
	t      *testing.T
	ts     *tunnel.Server
	srv    *httptest.Server
	mu     sync.Mutex
	cancel []context.CancelFunc
}

func newRelay(t *testing.T, resolve Resolver) *relayRig {
	t.Helper()
	ts := tunnel.NewServerAuth(func(r *http.Request) (string, error) {
		raw, ok := strings.CutPrefix(r.Header.Get(tunnel.AuthHeader), "Bearer ")
		if !ok {
			return "", io.EOF
		}
		device, _, ok := strings.Cut(raw, ":")
		if !ok || device == "" {
			return "", io.EOF
		}
		return device, nil
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/tunnel", ts.HandleTunnel)
	mux.Handle("/v1/approver/", Handler(ts, resolve))
	srv := httptest.NewServer(mux)
	r := &relayRig{t: t, ts: ts, srv: srv}
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, c := range r.cancel {
			c()
		}
		srv.Close()
	})
	return r
}

// companion connects one Companion whose handler echoes the request.
func (r *relayRig) companion(device string) {
	r.t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(seen{Device: device, Method: req.Method, Path: req.URL.Path,
			Auth: req.Header.Get("Authorization"), Body: string(body), Header: req.Header})
	})
	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.cancel = append(r.cancel, cancel)
	r.mu.Unlock()
	client := &tunnel.Client{RelayURL: "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/tunnel",
		Token: device + ":secret", Handler: handler, EagerSSE: true}
	go client.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !r.ts.DeviceConnected(device) {
		if time.Now().After(deadline) {
			r.t.Fatalf("device %s never connected", device)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *relayRig) do(method, path, auth, body string) (*http.Response, seen) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got seen
	json.Unmarshal(raw, &got)
	if got.Device == "" {
		got.Body = string(raw)
	}
	return resp, got
}

func signed(id string) string {
	return scheme + " " + id + ":1758300000:nonce0123456789ab:c2ln"
}

// TestEachCallReachesTheCompanionItsCredentialNames is the routing itself:
// two Companions behind one relay, and a phone paired with either reaches
// its own — with the method, path, header and body intact, a GET included,
// because the inbox is one.
func TestEachCallReachesTheCompanionItsCredentialNames(t *testing.T) {
	r := newRelay(t, ByDeviceID)
	r.companion("dev_a")
	r.companion("dev_b")

	resp, got := r.do(http.MethodGet, "/v1/approver/inbox?wait=1", signed("dev_a.apr_1"), "")
	if resp.StatusCode != http.StatusOK || got.Device != "dev_a" || got.Method != http.MethodGet || got.Path != "/v1/approver/inbox" {
		t.Fatalf("inbox for a: %d %+v", resp.StatusCode, got)
	}
	if got.Auth != signed("dev_a.apr_1") {
		t.Errorf("the credential must arrive as sent, got %q", got.Auth)
	}
	resp, got = r.do(http.MethodPost, "/v1/approver/answer", signed("dev_b.apr_2"), `{"handle":"h","approved":true}`)
	if resp.StatusCode != http.StatusOK || got.Device != "dev_b" || got.Body != `{"handle":"h","approved":true}` {
		t.Fatalf("answer for b: %d %+v", resp.StatusCode, got)
	}
	// A claim carries its key inside the code; it goes where the code was
	// minted and arrives whole, keys and all.
	claim := `{"code":"dev_b.Kx9","name":"Pixel","sign_pub":"AA","box_pub":"BB"}`
	resp, got = r.do(http.MethodPost, "/v1/approver/claim", "", claim)
	if resp.StatusCode != http.StatusOK || got.Device != "dev_b" || got.Body != claim {
		t.Fatalf("claim for b: %d %+v", resp.StatusCode, got)
	}
}

// TestACredentialWithoutAKeyIsRefusedBeforeAnyTunnel: nothing without a
// routing key is worth a Companion's time, and the refusal reads like the
// Companion's own so the page lands in the same state.
func TestACredentialWithoutAKeyIsRefusedBeforeAnyTunnel(t *testing.T) {
	r := newRelay(t, ByDeviceID)
	r.companion("dev_a")
	cases := []struct {
		name, method, path, auth, body string
		want                           int
		code                           string
	}{
		{"no header", http.MethodGet, "/v1/approver/inbox", "", "", http.StatusUnauthorized, "unauthorized"},
		{"other scheme", http.MethodGet, "/v1/approver/inbox", "Bearer x", "", http.StatusUnauthorized, "unauthorized"},
		{"undotted id", http.MethodGet, "/v1/approver/inbox", signed("apr_1"), "", http.StatusUnauthorized, "unauthorized"},
		{"empty key", http.MethodGet, "/v1/approver/inbox", signed(".apr_1"), "", http.StatusUnauthorized, "unauthorized"},
		{"undotted code", http.MethodPost, "/v1/approver/claim", "", `{"code":"Kx9"}`, http.StatusNotFound, "bad_code"},
		{"unparseable claim", http.MethodPost, "/v1/approver/claim", "", `{`, http.StatusBadRequest, "invalid_request"},
		{"unknown companion", http.MethodGet, "/v1/approver/inbox", signed("dev_zz.apr_1"), "", http.StatusServiceUnavailable, ""},
		{"too big", http.MethodPost, "/v1/approver/answer", signed("dev_a.apr_1"), strings.Repeat("x", maxBody+1), http.StatusRequestEntityTooLarge, "body_too_large"},
	}
	for _, tc := range cases {
		resp, got := r.do(tc.method, tc.path, tc.auth, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, resp.StatusCode, tc.want, got.Body)
		}
		if got.Device != "" {
			t.Errorf("%s: reached companion %s", tc.name, got.Device)
		}
		if tc.code != "" && !strings.Contains(got.Body, `"error":"`+tc.code+`"`) {
			t.Errorf("%s: body %q, want error %q", tc.name, got.Body, tc.code)
		}
	}
}

// TestTheSharedTokenRelayRoutesToItsOneCompanion: legacy mode has a single
// tunnel, and the key in the credential only has to be there.
func TestTheSharedTokenRelayRoutesToItsOneCompanion(t *testing.T) {
	r := newRelay(t, ToLegacy)
	r.companion(tunnel.LegacyIdentity)
	resp, got := r.do(http.MethodGet, "/v1/approver/inbox", signed("k.apr_1"), "")
	if resp.StatusCode != http.StatusOK || got.Device != tunnel.LegacyIdentity {
		t.Fatalf("inbox: %d %+v", resp.StatusCode, got)
	}
}

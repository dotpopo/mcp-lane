package approver

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/approval"
	"github.com/dotpopo/mcp-lane/companion/internal/store"
	"github.com/dotpopo/mcp-lane/companion/internal/txn"
)

// The secret the tests watch for: a line of a diff that must never appear
// anywhere but inside a sealed envelope.
const secretLine = "+DATABASE_URL=postgres://root:hunter2@db/prod"

// rig is a Companion's approval side with a controllable clock and a log the
// tests can search.
type rig struct {
	t      *testing.T
	svc    *Service
	appr   *approval.Service
	logBuf *bytes.Buffer
	push   *fakePush
	mu     sync.Mutex
	clock  time.Time
	asked  chan *approval.Pending
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fylane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &rig{t: t, logBuf: &bytes.Buffer{}, clock: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		asked: make(chan *approval.Pending, 8), push: newFakePush(t)}
	r.appr, err = approval.New(approval.ModeSafe, approval.DefaultBudgets(), func(p *approval.Pending) {
		r.asked <- p
		r.svc.Wake()
	})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r.svc, err = New(Options{
		Store:   st,
		Pending: r.appr.Pending,
		Resolve: r.appr.Resolve,
		View: func(p *approval.Pending) any {
			return map[string]any{"change_set_id": p.Request.ChangeSetID, "summary": p.Request.Summary,
				"operations": p.Request.Operations, "provider": p.Request.Provider, "kind": p.Request.Kind,
				"created_at": p.CreatedAt, "workspace_name": p.Request.WorkspaceName}
		},
		Signer:    signer,
		PublicURL: func() string { return "https://core.example" },
		Push:      r.push,
		Log:       slog.New(slog.NewTextHandler(r.logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:       r.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.appr.OnDecision = func(*approval.Pending, bool, time.Duration) { r.svc.Wake() }
	return r
}

func (r *rig) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clock
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.clock = r.clock.Add(d)
	r.mu.Unlock()
}

// ask raises one prompt the way a write would and returns the decision the
// waiting caller eventually receives.
func (r *rig) ask(id string) <-chan txn.Decision {
	out := make(chan txn.Decision, 1)
	req := &txn.ApprovalRequest{ChangeSetID: id, WorkspaceID: "ws", WorkspaceName: "demo", Provider: "claude",
		Summary: "Update .env.example", Kind: txn.KindWrite,
		Operations: []txn.OpPreview{{Type: txn.OpUpdate, Path: ".env.example", Diff: "--- a\n+++ b\n" + secretLine + "\n"}}}
	go func() {
		d, _ := r.appr.Approve(context.Background(), req)
		out <- d
	}()
	select {
	case <-r.asked:
	case <-time.After(2 * time.Second):
		r.t.Fatal("the prompt never reached the window")
	}
	return out
}

func (r *rig) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.svc.Handler().ServeHTTP(rec, req)
	return rec
}

// phone is the paired device, as the tests play it: two keys, a signed
// request per call, and the means to open what it is sent.
type phone struct {
	id      string
	sign    ed25519.PrivateKey
	box     *ecdh.PrivateKey
	corePub ed25519.PublicKey
}

func newPhone(t *testing.T) *phone {
	t.Helper()
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	box, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &phone{sign: sign, box: box}
}

func (p *phone) claim() ClaimRequest {
	return ClaimRequest{Name: "Pixel",
		SignPub: base64.RawURLEncoding.EncodeToString(p.sign.Public().(ed25519.PublicKey)),
		BoxPub:  base64.RawURLEncoding.EncodeToString(p.box.PublicKey().Bytes())}
}

// pair runs the whole pairing: desktop mints, phone claims.
func (r *rig) pair(p *phone) {
	r.t.Helper()
	pairing, err := r.svc.Pair()
	if err != nil {
		r.t.Fatal(err)
	}
	if !strings.HasPrefix(pairing.URL, "https://core.example/approver#") {
		r.t.Fatalf("pairing address %q does not carry the code as a fragment", pairing.URL)
	}
	// The code is one a person can read off the screen and type: three
	// groups of four from an alphabet without look-alikes.
	if !regexp.MustCompile(`^[23456789A-HJ-NP-Z]{4}-[23456789A-HJ-NP-Z]{4}-[23456789A-HJ-NP-Z]{4}$`).MatchString(pairing.Code) {
		r.t.Fatalf("pairing code %q is not a typed code", pairing.Code)
	}
	req := p.claim()
	req.Code = pairing.Code
	rec := r.do(httptest.NewRequest("POST", "/v1/approver/claim", jsonBody(req)))
	if rec.Code != http.StatusCreated {
		r.t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	var res ClaimResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		r.t.Fatal(err)
	}
	p.id = res.DeviceID
	pub, err := base64.RawURLEncoding.DecodeString(res.CoreSignPub)
	if err != nil {
		r.t.Fatal(err)
	}
	p.corePub = ed25519.PublicKey(pub)
}

// signed builds a request the Companion should accept: signed by this phone
// at the given moment with a fresh nonce.
func (p *phone) signed(method, target string, body []byte, at time.Time, nonce string) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	ts := strconv.FormatInt(at.Unix(), 10)
	if nonce == "" {
		nonce = randomToken(16)
	}
	sig := ed25519.Sign(p.sign, requestMessage(method, req.URL.Path, ts, nonce, body))
	req.Header.Set("Authorization", Scheme+" "+strings.Join([]string{p.id, ts, nonce,
		base64.RawURLEncoding.EncodeToString(sig)}, ":"))
	return req
}

// open verifies the Companion's signature and decrypts one envelope.
func (p *phone) open(t *testing.T, e Envelope) []byte {
	t.Helper()
	sig, _ := base64.RawURLEncoding.DecodeString(e.Sig)
	if !ed25519.Verify(p.corePub, envelopeMessage(p.id, e), sig) {
		t.Fatal("envelope is not signed by the Companion this phone paired with")
	}
	epk, _ := base64.RawURLEncoding.DecodeString(e.EphemeralPub)
	nonce, _ := base64.RawURLEncoding.DecodeString(e.Nonce)
	ct, _ := base64.RawURLEncoding.DecodeString(e.Ciphertext)
	pub, err := ecdh.P256().NewPublicKey(epk)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := p.box.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	boxPub := p.box.PublicKey().Bytes()
	key, err := hkdf.Key(sha256.New, shared, append(append([]byte{}, epk...), boxPub...), kdfInfo, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ct, []byte(p.id+"\n"+e.Handle))
	if err != nil {
		t.Fatalf("the phone could not open its own envelope: %v", err)
	}
	return plain
}

func jsonBody(v any) *bytes.Reader {
	raw, _ := json.Marshal(v)
	return bytes.NewReader(raw)
}

func inbox(t *testing.T, rec *httptest.ResponseRecorder) []Envelope {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("inbox: %d %s", rec.Code, rec.Body)
	}
	var res struct {
		Prompts []Envelope `json:"prompts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res.Prompts
}

func TestAPairingCodeIsGoodOnceAndForTenMinutes(t *testing.T) {
	r := newRig(t)
	if _, err := New(Options{}); err == nil {
		t.Fatal("a service without its wiring must not come up")
	}

	unpublished, _ := New(Options{Store: r.svc.opts.Store, Pending: r.appr.Pending, Resolve: r.appr.Resolve,
		View: r.svc.opts.View, Signer: r.svc.opts.Signer})
	if _, err := unpublished.Pair(); err != ErrNoPublicURL {
		t.Fatalf("a code for no address: %v", err)
	}

	p := newPhone(t)
	r.pair(p)
	if p.id == "" || len(p.corePub) != ed25519.PublicKeySize {
		t.Fatal("claim did not hand the phone an identity and the Companion's key")
	}
	devices, err := r.svc.Devices(context.Background())
	if err != nil || len(devices) != 1 || devices[0].Name != "Pixel" || devices[0].Expired ||
		!devices[0].ExpiresAt.Equal(r.now().Add(DeviceTTL)) {
		t.Fatalf("paired device not listed as expected: %+v %v", devices, err)
	}

	// Reuse, a lapsed code, a made-up code and a malformed key all refuse.
	pairing, _ := r.svc.Pair()
	req := newPhone(t).claim()
	req.Code = pairing.Code
	if rec := r.do(httptest.NewRequest("POST", "/v1/approver/claim", jsonBody(req))); rec.Code != http.StatusCreated {
		t.Fatalf("first claim: %d", rec.Code)
	}
	if rec := r.do(httptest.NewRequest("POST", "/v1/approver/claim", jsonBody(req))); rec.Code != http.StatusNotFound {
		t.Fatalf("a used code claimed again: %d", rec.Code)
	}
	late, _ := r.svc.Pair()
	r.advance(PairingTTL + time.Second)
	req.Code = late.Code
	if rec := r.do(httptest.NewRequest("POST", "/v1/approver/claim", jsonBody(req))); rec.Code != http.StatusNotFound {
		t.Fatalf("a lapsed code accepted: %d", rec.Code)
	}
	req.Code = "not-a-code"
	if rec := r.do(httptest.NewRequest("POST", "/v1/approver/claim", jsonBody(req))); rec.Code != http.StatusNotFound {
		t.Fatalf("an invented code accepted: %d", rec.Code)
	}
	fresh, _ := r.svc.Pair()
	req.Code = fresh.Code
	req.BoxPub = "AAAA"
	if rec := r.do(httptest.NewRequest("POST", "/v1/approver/claim", jsonBody(req))); rec.Code != http.StatusBadRequest {
		t.Fatalf("a key that is not a key accepted: %d", rec.Code)
	}
	if n, _ := r.svc.Devices(context.Background()); len(n) != 2 {
		t.Fatalf("refused claims must pair nothing; have %d devices", len(n))
	}
}

func TestAPromptReachesThePhoneOnlyAsCiphertext(t *testing.T) {
	r := newRig(t)
	p := newPhone(t)
	r.pair(p)
	decided := r.ask("cs_1")

	rec := r.do(p.signed("GET", "/v1/approver/inbox", nil, r.now(), ""))
	if strings.Contains(rec.Body.String(), secretLine) || strings.Contains(rec.Body.String(), "cs_1") ||
		strings.Contains(rec.Body.String(), ".env") {
		t.Fatal("the response carried the prompt in the clear")
	}
	envs := inbox(t, rec)
	if len(envs) != 1 {
		t.Fatalf("expected one sealed prompt, got %d", len(envs))
	}
	plain := p.open(t, envs[0])
	var view struct {
		ChangeSetID string `json:"change_set_id"`
		Operations  []struct {
			Diff string `json:"diff"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(plain, &view); err != nil {
		t.Fatal(err)
	}
	if view.ChangeSetID != "cs_1" || len(view.Operations) != 1 || !strings.Contains(view.Operations[0].Diff, secretLine) {
		t.Fatalf("the phone did not get the whole prompt, diff included: %s", plain)
	}

	// Another phone paired to the same Companion cannot open it: a prompt is
	// sealed for the device that asked for it.
	other := newPhone(t)
	r.pair(other)
	stranger := inbox(t, r.do(other.signed("GET", "/v1/approver/inbox", nil, r.now(), "")))
	if stranger[0].Handle != envs[0].Handle {
		t.Fatal("the same prompt should carry the same handle for every device")
	}
	if stranger[0].Ciphertext == envs[0].Ciphertext {
		t.Fatal("one ciphertext for two devices means it was not sealed to either")
	}
	other.id = p.id
	if _, err := ecdhOpen(other, envs[0]); err == nil {
		t.Fatal("a second device opened an envelope sealed for the first")
	}

	// The answer goes through the approval service, and the caller that was
	// waiting learns who answered.
	rec = r.do(p.signed("POST", "/v1/approver/answer", mustJSON(answerRequest{Handle: envs[0].Handle, Approved: true}), r.now(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rec.Code, rec.Body)
	}
	select {
	case d := <-decided:
		if !d.Approved || d.Reason != "approver:"+p.id {
			t.Fatalf("decision %+v does not name the device", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting write never got its answer")
	}
	if recent := r.svc.Recent(); len(recent) != 1 || recent[0].ChangeSetID != "cs_1" || recent[0].DeviceName != "Pixel" || !recent[0].Approved {
		t.Fatalf("recent answers: %+v", recent)
	}
	if rec := r.do(p.signed("POST", "/v1/approver/answer", mustJSON(answerRequest{Handle: envs[0].Handle, Approved: false}), r.now(), "")); rec.Code != http.StatusNotFound {
		t.Fatalf("a decided prompt answered again: %d", rec.Code)
	}
	if strings.Contains(r.logBuf.String(), secretLine) || strings.Contains(r.logBuf.String(), ".env") {
		t.Fatalf("the log carried prompt content:\n%s", r.logBuf)
	}
}

func TestEveryBadAnswerIsRefusedAndLeavesThePromptWhereItWas(t *testing.T) {
	r := newRig(t)
	p := newPhone(t)
	r.pair(p)
	r.ask("cs_2")
	handle := inbox(t, r.do(p.signed("GET", "/v1/approver/inbox", nil, r.now(), "")))[0].Handle
	body := mustJSON(answerRequest{Handle: handle, Approved: true})

	impostor := newPhone(t)
	impostor.id = p.id
	tampered := p.signed("POST", "/v1/approver/answer", body, r.now(), "")
	tampered.Body = io.NopCloser(jsonBody(answerRequest{Handle: handle, Approved: false}))

	replayed := p.signed("POST", "/v1/approver/answer", body, r.now(), "nonce-used-twice-0001")
	if rec := r.do(p.signed("GET", "/v1/approver/inbox", nil, r.now(), "nonce-used-twice-0001")); rec.Code != http.StatusOK {
		t.Fatalf("first use of the nonce: %d", rec.Code)
	}

	cases := []struct {
		name   string
		req    *http.Request
		status int
		code   string
	}{
		{"no credential", httptest.NewRequest("POST", "/v1/approver/answer", bytes.NewReader(body)), 401, "unauthorized"},
		{"signed by another key", impostor.signed("POST", "/v1/approver/answer", body, r.now(), ""), 401, "unauthorized"},
		{"body changed after signing", tampered, 401, "unauthorized"},
		{"nonce used before", replayed, 401, "unauthorized"},
		{"signed a minute ago", p.signed("POST", "/v1/approver/answer", body, r.now().Add(-answerWindow-time.Second), ""), 401, "unauthorized"},
		{"signed in the future", p.signed("POST", "/v1/approver/answer", body, r.now().Add(answerWindow+time.Second), ""), 401, "unauthorized"},
		{"prompt unknown", p.signed("POST", "/v1/approver/answer", mustJSON(answerRequest{Handle: "nothing-here", Approved: true}), r.now(), ""), 404, "not_pending"},
	}
	for _, c := range cases {
		rec := r.do(c.req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.code) {
			t.Errorf("%s: got %d %s, want %d %s", c.name, rec.Code, rec.Body, c.status, c.code)
		}
		if pending := r.appr.Pending(); len(pending) != 1 || pending[0].Request.ChangeSetID != "cs_2" {
			t.Fatalf("%s: the prompt did not stay pending", c.name)
		}
		if len(r.svc.Recent()) != 0 {
			t.Fatalf("%s: a refused answer was recorded", c.name)
		}
	}

	// A pairing past its expiry stops answering; the page is told which.
	r.advance(DeviceTTL + time.Second)
	rec := r.do(p.signed("POST", "/v1/approver/answer", body, r.now(), ""))
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("expired pairing: %d %s", rec.Code, rec.Body)
	}
	r.advance(-DeviceTTL - time.Second)

	// A revoked pairing stops answering at once, and its earlier inbox is
	// worth nothing: the handle it holds resolves nothing any more.
	if err := r.svc.Revoke(context.Background(), p.id); err != nil {
		t.Fatal(err)
	}
	rec = r.do(p.signed("POST", "/v1/approver/answer", body, r.now(), ""))
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "revoked") {
		t.Fatalf("revoked pairing: %d %s", rec.Code, rec.Body)
	}
	if rec := r.do(p.signed("GET", "/v1/approver/inbox", nil, r.now(), "")); rec.Code != 401 {
		t.Fatalf("revoked pairing still reads the inbox: %d", rec.Code)
	}
	if pending := r.appr.Pending(); len(pending) != 1 {
		t.Fatal("the prompt must still be waiting for the desktop")
	}
	if devices, _ := r.svc.Devices(context.Background()); len(devices) != 0 {
		t.Fatal("a revoked device is still listed")
	}
}

func TestAnInboxPollWakesForANewPromptAndEndsOnRevoke(t *testing.T) {
	r := newRig(t)
	p := newPhone(t)
	r.pair(p)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.do(p.signed("GET", "/v1/approver/inbox?wait=10", nil, r.now(), "")) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("an empty inbox returned before its wait was up")
	default:
	}
	r.ask("cs_3")
	select {
	case rec := <-done:
		if envs := inbox(t, rec); len(envs) != 1 {
			t.Fatalf("expected the new prompt, got %d", len(envs))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll did not wake for the prompt")
	}

	// Answer it from the desktop, then hold a poll open and revoke.
	r.appr.Resolve("cs_3", true, "")
	go func() { done <- r.do(p.signed("GET", "/v1/approver/inbox?wait=10", nil, r.now(), "")) }()
	time.Sleep(50 * time.Millisecond)
	if err := r.svc.Revoke(context.Background(), p.id); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-done:
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), "revoked") {
			t.Fatalf("poll after revoke: %d %s", rec.Code, rec.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll outlived the pairing")
	}
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// ecdhOpen is open without the fatal: the failure is the expected result.
func ecdhOpen(p *phone, e Envelope) ([]byte, error) {
	epk, _ := base64.RawURLEncoding.DecodeString(e.EphemeralPub)
	nonce, _ := base64.RawURLEncoding.DecodeString(e.Nonce)
	ct, _ := base64.RawURLEncoding.DecodeString(e.Ciphertext)
	pub, err := ecdh.P256().NewPublicKey(epk)
	if err != nil {
		return nil, err
	}
	shared, err := p.box.ECDH(pub)
	if err != nil {
		return nil, err
	}
	boxPub := p.box.PublicKey().Bytes()
	key, err := hkdf.Key(sha256.New, shared, append(append([]byte{}, epk...), boxPub...), kdfInfo, 32)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	return gcm.Open(nil, nonce, ct, []byte(p.id+"\n"+e.Handle))
}

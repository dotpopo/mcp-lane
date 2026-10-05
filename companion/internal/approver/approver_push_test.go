package approver

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/webpush"
)

// fakePush stands in for the push service: it records what it was asked
// to deliver and answers as told.
type fakePush struct {
	key  []byte
	sent chan pushSent
	mu   sync.Mutex
	fail error
}

type pushSent struct {
	sub     webpush.Subscription
	payload []byte
	ttl     time.Duration
	topic   string
}

func newFakePush(t *testing.T) *fakePush {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakePush{key: k.PublicKey().Bytes(), sent: make(chan pushSent, 8)}
}

func (f *fakePush) PublicKey() []byte { return f.key }

func (f *fakePush) Send(_ context.Context, sub webpush.Subscription, payload []byte, ttl time.Duration, topic string) error {
	f.mu.Lock()
	err := f.fail
	f.mu.Unlock()
	f.sent <- pushSent{sub, payload, ttl, topic}
	return err
}

func (f *fakePush) failWith(err error) {
	f.mu.Lock()
	f.fail = err
	f.mu.Unlock()
}

// subscription is what a phone's browser would hand over.
func (p *phone) subscription(t *testing.T, endpoint string) pushRequest {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	return pushRequest{Endpoint: endpoint,
		P256DH: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(auth)}
}

func (r *rig) pushInfo(p *phone) pushInfo {
	r.t.Helper()
	rec := r.do(p.signed("GET", "/v1/approver/push", nil, r.now(), ""))
	if rec.Code != http.StatusOK {
		r.t.Fatalf("push info: %d %s", rec.Code, rec.Body)
	}
	var info pushInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		r.t.Fatal(err)
	}
	return info
}

func (r *rig) subscribe(p *phone, req pushRequest) int {
	r.t.Helper()
	body, _ := json.Marshal(req)
	return r.do(p.signed("POST", "/v1/approver/push", body, r.now(), "")).Code
}

func (r *rig) noPushWithin(d time.Duration) {
	r.t.Helper()
	select {
	case got := <-r.push.sent:
		r.t.Fatalf("a push went out that should not have: %+v", got)
	case <-time.After(d):
	}
}

func (r *rig) pushed() pushSent {
	r.t.Helper()
	select {
	case got := <-r.push.sent:
		return got
	case <-time.After(2 * time.Second):
		r.t.Fatal("no push went out")
		return pushSent{}
	}
}

func TestAPhoneThatOptedInIsWokenWithNothingButAMarker(t *testing.T) {
	r := newRig(t)
	phone, quiet := newPhone(t), newPhone(t)
	r.pair(phone)
	r.pair(quiet)

	info := r.pushInfo(phone)
	if !info.Available || info.Enabled || info.Key != base64.RawURLEncoding.EncodeToString(r.push.key) {
		t.Fatalf("before opting in: %+v", info)
	}
	sub := phone.subscription(t, "https://push.example.net/s/phone")
	if code := r.subscribe(phone, sub); code != http.StatusOK {
		t.Fatalf("subscribe: %d", code)
	}
	if !r.pushInfo(phone).Enabled {
		t.Fatal("the subscription was not kept")
	}

	r.ask("cs_push")
	r.svc.Notify()
	got := r.pushed()
	if got.sub.Endpoint != sub.Endpoint || got.ttl != pushTTL || got.topic != pushTopic {
		t.Errorf("push %+v", got)
	}
	payload := string(got.payload)
	if payload != string(pushPayload) || strings.Contains(payload, secretLine) ||
		strings.Contains(payload, ".env") || strings.Contains(payload, "cs_push") || strings.Contains(payload, "claude") {
		t.Errorf("the push must carry a marker and nothing of the prompt: %q", payload)
	}
	// The phone that never opted in gets nothing.
	r.noPushWithin(200 * time.Millisecond)

	// Withdrawing stops it.
	if code := r.subscribe(phone, pushRequest{}); code != http.StatusOK {
		t.Fatalf("withdraw: %d", code)
	}
	if r.pushInfo(phone).Enabled {
		t.Fatal("withdrawn subscription still enabled")
	}
	r.svc.Notify()
	r.noPushWithin(200 * time.Millisecond)
}

func TestAPushSubscriptionIsSignedForAndChecked(t *testing.T) {
	r := newRig(t)
	phone := newPhone(t)
	r.pair(phone)
	good := phone.subscription(t, "https://push.example.net/s/phone")

	body, _ := json.Marshal(good)
	if rec := r.do(httptest.NewRequest("POST", "/v1/approver/push", bytes.NewReader(body))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unsigned subscription must be refused: %d", rec.Code)
	}
	bad := []pushRequest{
		{Endpoint: "http://push.example.net/s/phone", P256DH: good.P256DH, Auth: good.Auth},
		{Endpoint: good.Endpoint, P256DH: "AAAA", Auth: good.Auth},
		{Endpoint: good.Endpoint, P256DH: good.P256DH, Auth: "AAAA"},
		{Endpoint: good.Endpoint, P256DH: good.P256DH, Auth: "not base64!"},
	}
	for i, b := range bad {
		if code := r.subscribe(phone, b); code != http.StatusBadRequest {
			t.Errorf("subscription %d: %d, want 400", i, code)
		}
	}
	if r.pushInfo(phone).Enabled {
		t.Fatal("a refused subscription must leave nothing behind")
	}
	r.ask("cs_none")
	r.svc.Notify()
	r.noPushWithin(200 * time.Millisecond)
}

func TestAGoneSubscriptionIsForgottenAndAnExpiredPairingNotWoken(t *testing.T) {
	r := newRig(t)
	phone := newPhone(t)
	r.pair(phone)
	if code := r.subscribe(phone, phone.subscription(t, "https://push.example.net/s/phone")); code != http.StatusOK {
		t.Fatalf("subscribe: %d", code)
	}
	r.push.failWith(webpush.ErrGone)
	r.ask("cs_gone")
	r.svc.Notify()
	r.pushed()
	deadline := time.Now().Add(2 * time.Second)
	for r.pushInfo(phone).Enabled {
		if time.Now().After(deadline) {
			t.Fatal("a subscription the push service reported gone must be forgotten")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(r.logBuf.String(), "subscription gone") {
		t.Error("the log should say why the phone will be asked again")
	}

	r.push.failWith(nil)
	again := newPhone(t)
	r.pair(again)
	if code := r.subscribe(again, again.subscription(t, "https://push.example.net/s/again")); code != http.StatusOK {
		t.Fatalf("subscribe: %d", code)
	}
	r.advance(31 * 24 * time.Hour)
	r.svc.Notify()
	r.noPushWithin(200 * time.Millisecond)
}

func TestWithoutAKeyPushIsReportedAbsentNotBroken(t *testing.T) {
	r := newRig(t)
	r.svc.opts.Push = nil
	phone := newPhone(t)
	r.pair(phone)
	if info := r.pushInfo(phone); info.Available || info.Key != "" {
		t.Fatalf("no key means no push: %+v", info)
	}
	if code := r.subscribe(phone, phone.subscription(t, "https://push.example.net/s/phone")); code != http.StatusConflict {
		t.Fatalf("subscribing where there is no key: %d, want 409", code)
	}
	r.ask("cs_quiet")
	r.svc.Notify()
	r.noPushWithin(100 * time.Millisecond)
}

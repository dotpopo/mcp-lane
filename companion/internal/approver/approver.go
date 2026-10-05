// Package approver lets a device the user paired from the desktop — a phone,
// another computer — answer this Companion's approval prompts (D43).
//
// What moves is only where the user's finger lands. Who may answer is
// decided here by a key the user paired in person; what is executed, and
// where it is recorded, stays in this process: an answer from a device goes
// through the same Resolve as a click in the desktop window, and the audit
// row names the device. Nothing on the ladder is skipped, and no platform is
// ever the one answering.
//
// Prompts leave this process only encrypted to the device's own key: a diff
// crosses a tunnel, and in relay mode a relay, as ciphertext that neither can
// read. Each answer is a signed request that is good once and for a minute.
//
// The key agreement is ECDH over P-256 rather than X25519: a phone's Web
// Crypto has had P-256 for a decade, while X25519 reached Safari only in
// 18.4 — an iOS 17 phone could not pair at all (found 2026-09-19).
package approver

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/approval"
	"github.com/dotpopo/mcp-lane/companion/internal/store"
)

const (
	// PairingTTL is how long a pairing code shown on the desktop stays
	// claimable. The user is holding the phone up to the screen; ten minutes
	// is generous for that and short for anyone else.
	PairingTTL = 10 * time.Minute
	// DeviceTTL bounds a pairing. The device's private keys live in browser
	// storage rather than an OS keychain, and an expiry the user has to renew
	// by scanning again is the cheapest compensation for that.
	DeviceTTL = 30 * 24 * time.Hour
	// answerWindow is how far a signed request's timestamp may sit from this
	// machine's clock and still count. A replay inside the window is caught
	// by the nonce; outside it, by the clock.
	answerWindow = 60 * time.Second
	// maxWait caps an inbox long-poll so an idle connection is cheap to hold
	// and cheap to lose.
	maxWait = 25 * time.Second
	// Scheme is the Authorization scheme of a signed device request.
	Scheme = "Fylane-Approver"

	envelopeTag = "fylane-approver-envelope-v1"
	requestTag  = "fylane-approver-request-v1"
	kdfInfo     = "fylane-approver-v1"
	maxBody     = 64 << 10
)

// Store is what the service needs persisted: the paired devices. Implemented
// by *store.Store.
type Store interface {
	PutApprover(context.Context, *store.ApproverDevice) error
	GetApprover(context.Context, string) (*store.ApproverDevice, error)
	ListApprovers(context.Context) ([]*store.ApproverDevice, error)
	DeleteApprover(context.Context, string) error
	SetApproverPush(ctx context.Context, id, endpoint string, p256dh, auth []byte) error
}

// Options wires a Service to the approval authority it answers for.
type Options struct {
	Store Store
	// Pending lists the prompts awaiting a decision; Resolve decides one.
	// Both come from the approval service — the same functions the desktop
	// window uses, on purpose.
	Pending func() []*approval.Pending
	Resolve func(changeSetID string, approved bool, reason string) bool
	// View renders one prompt the way the desktop shows it, so the phone
	// says the same things in the same words. Whatever it returns is what
	// gets encrypted; it must marshal to JSON.
	View func(*approval.Pending) any
	// Signer signs every envelope so the device can tell a prompt from this
	// Companion apart from one a relay made up. Its public half is handed to
	// the device when it pairs.
	Signer ed25519.PrivateKey
	// PublicURL is where the device reaches this Companion; the pairing
	// address is built on it. Empty means no tunnel is up yet.
	PublicURL func() string
	// RouteKey, when set, prefixes every pairing code and device id this
	// Companion hands out with "<key>.", so a relay in front of many
	// Companions can tell from the credential alone which tunnel a request
	// belongs in (V-T4). It is this machine's relay device id. The relay
	// keeps no table: the prefix is the whole routing state, and a device
	// that reaches the wrong Companion is simply not in its store. Empty in
	// direct mode, where nothing sits in front.
	RouteKey string
	// Push, when set, wakes a subscribed phone when a prompt arrives (V-T5).
	// Nil means the feature is absent: the page still works, it just has
	// to be open.
	Push Pusher
	Log  *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Service is the approver surface: pairing, the encrypted inbox, signed
// answers, and the device list the desktop shows.
type Service struct {
	opts Options
	now  func() time.Time

	mu sync.Mutex
	// codes are the unclaimed pairing codes and when each lapses.
	codes map[string]time.Time
	// nonces are the request nonces seen per device inside the window; a
	// second use is a replay. Entries outlive the window and are swept.
	nonces map[string]map[string]time.Time
	// recent are the last answers devices gave, for the desktop to say who
	// answered a prompt it watched disappear.
	recent []Answer
	// handleKey makes prompt handles opaque outside this process: a change
	// set id can carry a file name, and the answer travels in the clear.
	handleKey []byte
	// wake is closed and replaced whenever the pending set may have changed,
	// which is what an inbox long-poll waits on.
	wake chan struct{}
}

// New returns a ready Service. Every option except Now and Log is required.
func New(opts Options) (*Service, error) {
	if opts.Store == nil || opts.Pending == nil || opts.Resolve == nil || opts.View == nil {
		return nil, errors.New("approver: store, pending, resolve and view are required")
	}
	if len(opts.Signer) != ed25519.PrivateKeySize {
		return nil, errors.New("approver: a signing key is required")
	}
	// The key is split off at the first dot and travels in a colon-joined
	// header; either character inside it would route or parse wrong.
	if strings.ContainsAny(opts.RouteKey, ".:/ \t\r\n") {
		return nil, fmt.Errorf("approver: route key %q holds a separator", opts.RouteKey)
	}
	if opts.PublicURL == nil {
		opts.PublicURL = func() string { return "" }
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("approver: reading random bytes: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{opts: opts, now: now, codes: map[string]time.Time{},
		nonces: map[string]map[string]time.Time{}, handleKey: key, wake: make(chan struct{})}, nil
}

// Device is one paired device as the desktop lists it.
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Expired   bool      `json:"expired"`
}

// Answer is one decision a device made.
type Answer struct {
	ChangeSetID string    `json:"change_set_id"`
	DeviceID    string    `json:"device_id"`
	DeviceName  string    `json:"device_name"`
	Approved    bool      `json:"approved"`
	At          time.Time `json:"at"`
}

// Pairing is what the desktop turns into a QR code.
type Pairing struct {
	Code string `json:"code"`
	// URL is the page the device opens, code included as the fragment so it
	// never reaches a server log.
	URL       string        `json:"url"`
	ExpiresIn time.Duration `json:"-"`
}

// ErrNoPublicURL is returned by Pair while no tunnel has published this
// Companion: a code for an address that resolves to nothing would only
// confuse.
var ErrNoPublicURL = errors.New("no public address yet: start the tunnel first")

// Pair mints a single-use pairing code. Local callers only — the desktop
// over the control API. Minting from the public surface would let a
// stranger pair themselves to the user's approvals.
func (s *Service) Pair() (Pairing, error) {
	base := strings.TrimRight(s.opts.PublicURL(), "/")
	if base == "" {
		return Pairing{}, ErrNoPublicURL
	}
	code := s.routed(humanCode())
	s.mu.Lock()
	now := s.now()
	for c, exp := range s.codes {
		if now.After(exp) {
			delete(s.codes, c)
		}
	}
	s.codes[normalizeCode(code)] = now.Add(PairingTTL)
	s.mu.Unlock()
	return Pairing{Code: code, URL: base + "/approver#" + code, ExpiresIn: PairingTTL}, nil
}

// codeAlphabet is the platform pairing code's: no 0/O or 1/I, one case.
// A code is read off a screen and typed on a phone when the camera cannot
// be used, so it must survive being read aloud and typed in lowercase.
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// humanCode is twelve letters in three groups, 60 bits. The code lives ten
// minutes behind a per-address limit of a few calls a second, so a guess
// has no chance at that strength, and no strength above it helps.
func humanCode() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("approver: reading random bytes: %v", err))
	}
	out := make([]byte, 0, 14)
	for i, b := range buf {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, codeAlphabet[int(b)%len(codeAlphabet)])
	}
	return string(out)
}

// normalizeCode is how a code is keyed: the route key, if any, exactly as
// minted, and the code itself without its grouping dashes and in upper
// case, so that what a person typed matches what the desktop showed.
func normalizeCode(code string) string {
	code = strings.TrimSpace(code)
	key, rest, ok := strings.Cut(code, ".")
	if !ok {
		key, rest = "", code
	}
	rest = strings.ToUpper(strings.ReplaceAll(rest, "-", ""))
	if !ok {
		return rest
	}
	return key + "." + rest
}

// routed prefixes a code or id with the route key, when there is one.
func (s *Service) routed(id string) string {
	if s.opts.RouteKey == "" {
		return id
	}
	return s.opts.RouteKey + "." + id
}

// takeCode consumes a pairing code; false when it is unknown or lapsed.
func (s *Service) takeCode(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	code = normalizeCode(code)
	exp, ok := s.codes[code]
	if !ok {
		return false
	}
	delete(s.codes, code)
	return !s.now().After(exp)
}

// ClaimRequest is what a device sends with a pairing code: its name and the
// public halves of the two keys it just generated — Ed25519 for signing,
// P-256 (uncompressed point) for receiving.
type ClaimRequest struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	SignPub string `json:"sign_pub"`
	BoxPub  string `json:"box_pub"`
}

// ClaimResponse is the device's identity from here on, plus the key it
// verifies envelopes with.
type ClaimResponse struct {
	DeviceID    string    `json:"device_id"`
	CoreSignPub string    `json:"core_sign_pub"`
	ExpiresAt   time.Time `json:"expires_at"`
}

var (
	// ErrBadCode covers an unknown, used or lapsed pairing code.
	ErrBadCode = errors.New("pairing code is unknown or has expired")
	// ErrBadKey covers a public key that is not the right shape.
	ErrBadKey = errors.New("device keys are not valid")
)

// Claim pairs a device: the code proves it was shown this Companion's
// screen, the keys are what it will be known by.
func (s *Service) Claim(ctx context.Context, req ClaimRequest) (ClaimResponse, error) {
	signPub, err := base64.RawURLEncoding.DecodeString(req.SignPub)
	if err != nil || len(signPub) != ed25519.PublicKeySize {
		return ClaimResponse{}, ErrBadKey
	}
	boxPub, err := base64.RawURLEncoding.DecodeString(req.BoxPub)
	if err != nil {
		return ClaimResponse{}, ErrBadKey
	}
	if _, err := ecdh.P256().NewPublicKey(boxPub); err != nil {
		return ClaimResponse{}, ErrBadKey
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Approver"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	// The code is consumed before anything is stored, so a race between two
	// claims of the same code pairs at most one device.
	if !s.takeCode(req.Code) {
		return ClaimResponse{}, ErrBadCode
	}
	now := s.now()
	d := &store.ApproverDevice{ID: s.routed("apr_" + randomToken(12)), Name: name, SignPub: signPub, BoxPub: boxPub,
		CreatedAt: now, ExpiresAt: now.Add(DeviceTTL)}
	if err := s.opts.Store.PutApprover(ctx, d); err != nil {
		return ClaimResponse{}, err
	}
	s.opts.Log.Info("approver paired", "device_id", d.ID)
	return ClaimResponse{DeviceID: d.ID, ExpiresAt: d.ExpiresAt,
		CoreSignPub: base64.RawURLEncoding.EncodeToString(s.opts.Signer.Public().(ed25519.PublicKey))}, nil
}

// Devices lists every pairing, lapsed ones marked.
func (s *Service) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.opts.Store.ListApprovers(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]Device, 0, len(rows))
	for _, r := range rows {
		out = append(out, Device{ID: r.ID, Name: r.Name, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
			Expired: now.After(r.ExpiresAt)})
	}
	return out, nil
}

// Revoke ends a pairing at once: the row goes, the device's nonces go, and
// every inbox poll it holds open is woken to find it gone.
func (s *Service) Revoke(ctx context.Context, id string) error {
	if err := s.opts.Store.DeleteApprover(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.nonces, id)
	s.mu.Unlock()
	s.opts.Log.Info("approver revoked", "device_id", id)
	s.Wake()
	return nil
}

// Recent returns the answers devices gave in the last few minutes, newest
// first.
func (s *Service) Recent() []Answer {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-10 * time.Minute)
	out := make([]Answer, 0, len(s.recent))
	for i := len(s.recent) - 1; i >= 0; i-- {
		if s.recent[i].At.After(cutoff) {
			out = append(out, s.recent[i])
		}
	}
	return out
}

// Wake tells waiting inbox polls that the pending set may have changed. The
// app calls it when a prompt arrives and when one is decided.
func (s *Service) Wake() {
	s.mu.Lock()
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) wakeCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake
}

// Handler serves the public surface: the page a device runs, and the API
// it talks to — claim, inbox, answer. Mount it on the direct-mode mux.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.PageRoutes(mux)
	mux.HandleFunc("POST /v1/approver/claim", s.handleClaim)
	mux.HandleFunc("GET /v1/approver/inbox", s.handleInbox)
	mux.HandleFunc("POST /v1/approver/answer", s.handleAnswer)
	mux.HandleFunc("GET /v1/approver/push", s.handlePushInfo)
	mux.HandleFunc("POST /v1/approver/push", s.handlePushSet)
	mux.HandleFunc("POST /v1/approver/forget", s.handleForget)
	return mux
}

// handleForget lets a device end its own pairing: the phone's "forget this
// pairing" reaches the computer's list too, instead of leaving a row that
// nothing will ever answer from. Only the device itself can do it — the
// request is signed like any other — and it revokes exactly one pairing.
func (s *Service) handleForget(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body")
		return
	}
	dev, err := s.authenticate(r.Context(), r, body)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if err := s.Revoke(r.Context(), dev.ID); err != nil {
		s.opts.Log.Warn("forgetting a pairing", "device", dev.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "store_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"forgotten": true})
}

func (s *Service) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	res, err := s.Claim(r.Context(), req)
	switch {
	case errors.Is(err, ErrBadKey):
		writeError(w, http.StatusBadRequest, "bad_key")
	case errors.Is(err, ErrBadCode):
		writeError(w, http.StatusNotFound, "bad_code")
	case err != nil:
		s.opts.Log.Warn("approver claim failed", "error", err)
		writeError(w, http.StatusInternalServerError, "store_unavailable")
	default:
		writeJSON(w, http.StatusCreated, res)
	}
}

// authError is why a signed request was refused. The code is all the device
// learns; "revoked" and "expired" are named because the page has to tell the
// user to pair again, and everything else is the one word.
type authError struct {
	status int
	code   string
}

func (e *authError) Error() string { return e.code }

var (
	errUnauthorized = &authError{http.StatusUnauthorized, "unauthorized"}
	errRevoked      = &authError{http.StatusUnauthorized, "revoked"}
	errExpired      = &authError{http.StatusUnauthorized, "expired"}
)

// authenticate verifies a signed device request. The signature covers the
// method, the path, a timestamp, a nonce and the body's digest, so a
// captured request cannot be replayed, altered, or pointed elsewhere.
// Nothing is recorded until the signature has been checked: a refusal
// leaves no state behind.
func (s *Service) authenticate(ctx context.Context, r *http.Request, body []byte) (*store.ApproverDevice, error) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, Scheme+" ") {
		return nil, errUnauthorized
	}
	parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(header, Scheme+" ")), ":")
	if len(parts) != 4 {
		return nil, errUnauthorized
	}
	deviceID, tsRaw, nonce, sigRaw := parts[0], parts[1], parts[2], parts[3]
	if len(nonce) < 16 || len(nonce) > 64 {
		return nil, errUnauthorized
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return nil, errUnauthorized
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigRaw)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errUnauthorized
	}
	now := s.now()
	if d := now.Sub(time.Unix(ts, 0)); d > answerWindow || d < -answerWindow {
		return nil, errUnauthorized
	}
	dev, err := s.opts.Store.GetApprover(ctx, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errRevoked
	}
	if err != nil {
		return nil, &authError{http.StatusServiceUnavailable, "store_unavailable"}
	}
	if now.After(dev.ExpiresAt) {
		return nil, errExpired
	}
	msg := requestMessage(r.Method, r.URL.Path, tsRaw, nonce, body)
	if !ed25519.Verify(ed25519.PublicKey(dev.SignPub), msg, sig) {
		return nil, errUnauthorized
	}
	if !s.noteNonce(deviceID, nonce, now) {
		return nil, errUnauthorized
	}
	return dev, nil
}

// requestMessage is the byte string a device signs. The body digest rather
// than the body keeps the message small and the format the same for a GET.
func requestMessage(method, path, ts, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{requestTag, method, path, ts, nonce, hex.EncodeToString(sum[:])}, "\n"))
}

// noteNonce records a nonce as used; false when it already was.
func (s *Service) noteNonce(deviceID, nonce string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.nonces[deviceID]
	if seen == nil {
		seen = map[string]time.Time{}
		s.nonces[deviceID] = seen
	}
	for n, at := range seen {
		if now.Sub(at) > 2*answerWindow {
			delete(seen, n)
		}
	}
	if _, dup := seen[nonce]; dup {
		return false
	}
	seen[nonce] = now
	return true
}

// Envelope is one prompt as it leaves this process: sealed to the device's
// key, signed by this Companion's. The handle is the only thing about the
// prompt that is readable outside, and it says nothing.
type Envelope struct {
	Handle       string `json:"handle"`
	EphemeralPub string `json:"epk"`
	Nonce        string `json:"nonce"`
	Ciphertext   string `json:"ct"`
	Sig          string `json:"sig"`
}

func (s *Service) handle(changeSetID string) string {
	mac := hmac.New(sha256.New, s.handleKey)
	mac.Write([]byte(changeSetID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}

// envelopes seals every pending prompt for one device.
func (s *Service) envelopes(dev *store.ApproverDevice) ([]Envelope, error) {
	pending := s.opts.Pending()
	out := make([]Envelope, 0, len(pending))
	for _, p := range pending {
		plain, err := json.Marshal(s.opts.View(p))
		if err != nil {
			return nil, fmt.Errorf("rendering prompt: %w", err)
		}
		h := s.handle(p.Request.ChangeSetID)
		epk, nonce, ct, err := seal(dev.BoxPub, []byte(dev.ID+"\n"+h), plain)
		if err != nil {
			return nil, err
		}
		e := Envelope{Handle: h,
			EphemeralPub: base64.RawURLEncoding.EncodeToString(epk),
			Nonce:        base64.RawURLEncoding.EncodeToString(nonce),
			Ciphertext:   base64.RawURLEncoding.EncodeToString(ct)}
		e.Sig = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.opts.Signer, envelopeMessage(dev.ID, e)))
		out = append(out, e)
	}
	return out, nil
}

// envelopeMessage is what this Companion signs on an envelope: the recipient
// and every field, so a relay can neither redirect one nor rewrite it.
func envelopeMessage(deviceID string, e Envelope) []byte {
	return []byte(strings.Join([]string{envelopeTag, deviceID, e.Handle, e.EphemeralPub, e.Nonce, e.Ciphertext}, "\n"))
}

// seal encrypts plaintext to a P-256 public key: an ephemeral key per
// message, HKDF-SHA256 over the shared secret with both public keys as salt,
// AES-256-GCM with aad bound in. Standard library throughout.
func seal(boxPub, aad, plaintext []byte) (epk, nonce, ct []byte, err error) {
	curve := ecdh.P256()
	pub, err := curve.NewPublicKey(boxPub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("device key: %w", err)
	}
	eph, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return nil, nil, nil, err
	}
	epk = eph.PublicKey().Bytes()
	key, err := hkdf.Key(sha256.New, shared, append(append([]byte{}, epk...), boxPub...), kdfInfo, 32)
	if err != nil {
		return nil, nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, nil, err
	}
	return epk, nonce, gcm.Seal(nil, nonce, plaintext, aad), nil
}

// handleInbox answers with every pending prompt sealed for the caller. With
// ?wait=<seconds> it holds the request open until one arrives or the wait
// runs out, re-checking the pairing each time it wakes so a revoke ends the
// poll rather than feeding it one more prompt.
func (s *Service) handleInbox(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticate(r.Context(), r, nil)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait") + "s")
	if wait > maxWait {
		wait = maxWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		wake := s.wakeCh()
		envs, err := s.envelopes(dev)
		if err != nil {
			s.opts.Log.Warn("sealing prompts", "error", err)
			writeError(w, http.StatusInternalServerError, "seal_failed")
			return
		}
		if len(envs) > 0 || wait <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"prompts": envs, "expires_at": dev.ExpiresAt})
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			writeJSON(w, http.StatusOK, map[string]any{"prompts": envs, "expires_at": dev.ExpiresAt})
			return
		case <-r.Context().Done():
			return
		}
		if dev, err = s.opts.Store.GetApprover(r.Context(), dev.ID); err != nil || s.now().After(dev.ExpiresAt) {
			writeAuthError(w, errRevoked)
			return
		}
	}
}

// answerRequest is a device's decision on one prompt.
type answerRequest struct {
	Handle   string `json:"handle"`
	Approved bool   `json:"approved"`
}

// handleAnswer records a device's decision through the same Resolve the
// desktop uses. A prompt that is no longer pending — answered elsewhere,
// or never known — is refused and nothing is written.
func (s *Service) handleAnswer(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	dev, err := s.authenticate(r.Context(), r, body)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	var req answerRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Handle == "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var changeSetID string
	for _, p := range s.opts.Pending() {
		if s.handle(p.Request.ChangeSetID) == req.Handle {
			changeSetID = p.Request.ChangeSetID
			break
		}
	}
	if changeSetID == "" || !s.opts.Resolve(changeSetID, req.Approved, "approver:"+dev.ID) {
		writeError(w, http.StatusNotFound, "not_pending")
		return
	}
	s.mu.Lock()
	s.recent = append(s.recent, Answer{ChangeSetID: changeSetID, DeviceID: dev.ID, DeviceName: dev.Name,
		Approved: req.Approved, At: s.now()})
	if len(s.recent) > 50 {
		s.recent = s.recent[len(s.recent)-50:]
	}
	s.mu.Unlock()
	s.opts.Log.Info("approval answered by device", "device_id", dev.ID, "approved", req.Approved)
	s.Wake()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeAuthError(w http.ResponseWriter, err error) {
	var ae *authError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.code)
		return
	}
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("approver: reading random bytes: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

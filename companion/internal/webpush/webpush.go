// Package webpush delivers a Web Push message to a browser's push service:
// RFC 8291 message encryption (aes128gcm, RFC 8188) and RFC 8292 VAPID
// authentication, on the standard library alone.
//
// The Companion uses it for one thing — waking a paired phone when a prompt
// is waiting — so the surface is one Send. The push service is an address
// the phone handed over; it is dialed under the same IP policy as any other
// outbound fetch, and never followed through a redirect.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/urlfetch"
)

// Subscription is what a browser's PushManager produced: where to post, and
// the two values the message is encrypted to.
type Subscription struct {
	Endpoint string
	// P256DH is the browser's public key, a 65-byte uncompressed point.
	P256DH []byte
	// Auth is the 16-byte secret the browser made for this subscription.
	Auth []byte
}

// Validate reports whether the subscription is one this package can send
// to: an https endpoint, a real P-256 point, a 16-byte auth secret.
func (s Subscription) Validate() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("endpoint must be an https address")
	}
	if _, err := ecdh.P256().NewPublicKey(s.P256DH); err != nil {
		return errors.New("p256dh is not a P-256 public key")
	}
	if len(s.Auth) != authSize {
		return fmt.Errorf("auth must be %d bytes", authSize)
	}
	return nil
}

// ErrGone is returned when the push service says the subscription no
// longer exists; the caller should forget it.
var ErrGone = errors.New("push subscription is gone")

const (
	authSize   = 16
	saltSize   = 16
	recordSize = 4096
	// subject is the contact the push service may use about this sender,
	// as VAPID requires one.
	subject = "https://github.com/dotpopo/mcp-lane"
	// tokenLife is how long a VAPID token is good for; the maximum allowed
	// is 24 hours, and one is minted per send anyway.
	tokenLife = 12 * time.Hour
)

// Sender posts messages under one VAPID key. The zero value is not usable;
// use NewSender.
type Sender struct {
	key       *ecdsa.PrivateKey
	checkIP   func(net.IP) error
	tlsConfig *tls.Config
	now       func() time.Time
}

// NewSender returns a Sender signing with key, a P-256 ECDSA key.
func NewSender(key *ecdsa.PrivateKey) (*Sender, error) {
	if key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("webpush: a P-256 key is required")
	}
	return &Sender{key: key, now: time.Now}, nil
}

// PublicKey is the application server key a browser subscribes with: the
// uncompressed P-256 point, 65 bytes.
func (s *Sender) PublicKey() []byte {
	return elliptic.Marshal(elliptic.P256(), s.key.PublicKey.X, s.key.PublicKey.Y)
}

// Send encrypts plaintext to the subscription and posts it. ttl bounds how
// long the push service holds the message for a phone that is offline;
// topic lets a newer message replace an older undelivered one.
func (s *Sender) Send(ctx context.Context, sub Subscription, plaintext []byte, ttl time.Duration, topic string) error {
	if err := sub.Validate(); err != nil {
		return fmt.Errorf("webpush: %w", err)
	}
	body, err := Encrypt(sub, plaintext)
	if err != nil {
		return err
	}
	endpoint, _ := url.Parse(sub.Endpoint)
	auth, err := s.vapid(endpoint.Scheme+"://"+endpoint.Host, s.now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webpush: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	req.Header.Set("Urgency", "high")
	if topic != "" {
		req.Header.Set("Topic", topic)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("webpush: posting to the push service: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	default:
		return fmt.Errorf("webpush: push service answered %d", resp.StatusCode)
	}
}

// client dials under the outbound IP policy, follows no redirect, and gives
// the push service ten seconds.
func (s *Sender) client() *http.Client {
	check := s.checkIP
	if check == nil {
		check = urlfetch.RejectNonPublic
	}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("cannot verify dial target %q", host)
			}
			if err := check(ip); err != nil {
				return fmt.Errorf("refusing to push to %s: %w", host, err)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:     dialer.DialContext,
			TLSClientConfig: s.tlsConfig,
			// The proxy env is ignored on purpose: a proxy would bypass the
			// dial-time IP policy.
			Proxy: nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirect refused")
		},
	}
}

// vapid mints the Authorization header for one origin: a JWT signed with
// the sender key, and the key itself for the service to check it with.
func (s *Sender) vapid(audience string, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": audience, "exp": now.Add(tokenLife).Unix(), "sub": subject})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("webpush: signing the token: %w", err)
	}
	// JWS wants the raw r||s, each left-padded to the curve size.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	token := signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	return "vapid t=" + token + ", k=" + base64.RawURLEncoding.EncodeToString(s.PublicKey()), nil
}

// Encrypt seals plaintext for the subscription as one aes128gcm record
// (RFC 8291 §3 with RFC 8188 framing) under a fresh sender key and salt.
func Encrypt(sub Subscription, plaintext []byte) ([]byte, error) {
	asKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: generating the message key: %w", err)
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("webpush: reading random bytes: %w", err)
	}
	return encryptWith(sub, plaintext, asKey, salt)
}

// encryptWith is Encrypt with the ephemeral inputs chosen by the caller, so
// the RFC's worked example can be reproduced byte for byte.
func encryptWith(sub Subscription, plaintext []byte, asKey *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext)+1+16 > recordSize {
		return nil, errors.New("webpush: message too long for one record")
	}
	uaPub, err := ecdh.P256().NewPublicKey(sub.P256DH)
	if err != nil {
		return nil, fmt.Errorf("webpush: %w", err)
	}
	shared, err := asKey.ECDH(uaPub)
	if err != nil {
		return nil, fmt.Errorf("webpush: agreeing on a key: %w", err)
	}
	asPub := asKey.PublicKey().Bytes()
	// IKM = HKDF(auth, ecdh_secret, "WebPush: info" || 0x00 || ua_public || as_public, 32)
	prkKey, err := hkdf.Extract(sha256.New, shared, sub.Auth)
	if err != nil {
		return nil, err
	}
	keyInfo := append(append([]byte("WebPush: info\x00"), sub.P256DH...), asPub...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// One record: the plaintext, then the delimiter that marks it last.
	record := append(append([]byte{}, plaintext...), 0x02)
	out := make([]byte, 0, saltSize+4+1+len(asPub)+len(record)+gcm.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	return gcm.Seal(out, nonce, record, nil), nil
}

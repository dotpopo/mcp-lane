package approver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/webpush"
)

// Pusher wakes a phone through its browser's push service. Implemented by
// *webpush.Sender; absent when this Companion has no key to sign with.
type Pusher interface {
	PublicKey() []byte
	Send(ctx context.Context, sub webpush.Subscription, payload []byte, ttl time.Duration, topic string) error
}

const (
	// pushTTL is how long the push service may hold a wake-up for a phone
	// that is offline. A prompt older than this has most likely been
	// answered on the computer.
	pushTTL = 5 * time.Minute
	// pushTopic makes a newer wake-up replace an older one still queued,
	// so a phone that was away gets one notification, not one per prompt.
	pushTopic = "fylane-approval"
	// pushTimeout bounds one delivery attempt; the goroutine it runs in
	// ends with it.
	pushTimeout = 10 * time.Second
)

// pushPayload is the whole of what a push carries. The prompt is never in
// it: the page fetches that, sealed to the phone, when it opens (D43 ⑤ —
// title only). The service worker does not even read this.
var pushPayload = []byte(`{"kind":"prompt"}`)

// pushInfo is what a device learns before subscribing: whether pushing is
// possible, the key to subscribe with, and whether it already has.
type pushInfo struct {
	Available bool   `json:"available"`
	Key       string `json:"key,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// pushRequest is a device's subscription, as PushManager.subscribe gave it.
// An empty endpoint withdraws it.
type pushRequest struct {
	Endpoint string `json:"endpoint"`
	P256DH   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

func (s *Service) handlePushInfo(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticate(r.Context(), r, nil)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	info := pushInfo{Available: s.opts.Push != nil, Enabled: dev.PushEndpoint != ""}
	if info.Available {
		info.Key = base64.RawURLEncoding.EncodeToString(s.opts.Push.PublicKey())
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Service) handlePushSet(w http.ResponseWriter, r *http.Request) {
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
	if s.opts.Push == nil {
		writeError(w, http.StatusConflict, "push_unavailable")
		return
	}
	var req pushRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_body")
		return
	}
	var p256dh, auth []byte
	if req.Endpoint != "" {
		p256dh, err = base64.RawURLEncoding.DecodeString(req.P256DH)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_subscription")
			return
		}
		auth, err = base64.RawURLEncoding.DecodeString(req.Auth)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_subscription")
			return
		}
		sub := webpush.Subscription{Endpoint: req.Endpoint, P256DH: p256dh, Auth: auth}
		if err := sub.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "bad_subscription")
			return
		}
	}
	if err := s.opts.Store.SetApproverPush(r.Context(), dev.ID, req.Endpoint, p256dh, auth); err != nil {
		s.opts.Log.Warn("storing push subscription", "device", dev.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "store_failed")
		return
	}
	s.opts.Log.Info("approver push "+map[bool]string{true: "enabled", false: "withdrawn"}[req.Endpoint != ""], "device", dev.ID)
	writeJSON(w, http.StatusOK, pushInfo{Available: true, Enabled: req.Endpoint != ""})
}

// Notify wakes every subscribed, unexpired device: a prompt has arrived.
// The app calls it when one does. Deliveries run in the background, each
// bounded by pushTimeout; a subscription the push service reports gone is
// forgotten so the phone is asked to subscribe again next time it opens.
func (s *Service) Notify() {
	if s.opts.Push == nil {
		return
	}
	devices, err := s.opts.Store.ListApprovers(context.Background())
	if err != nil {
		s.opts.Log.Warn("listing devices to notify", "error", err)
		return
	}
	now := s.now()
	for _, dev := range devices {
		if dev.PushEndpoint == "" || now.After(dev.ExpiresAt) {
			continue
		}
		go s.push(dev.ID, webpush.Subscription{Endpoint: dev.PushEndpoint, P256DH: dev.PushP256DH, Auth: dev.PushAuth})
	}
}

func (s *Service) push(deviceID string, sub webpush.Subscription) {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	err := s.opts.Push.Send(ctx, sub, pushPayload, pushTTL, pushTopic)
	switch {
	case err == nil:
	case errors.Is(err, webpush.ErrGone):
		if err := s.opts.Store.SetApproverPush(context.Background(), deviceID, "", nil, nil); err != nil {
			s.opts.Log.Warn("forgetting a gone push subscription", "device", deviceID, "error", err)
			return
		}
		s.opts.Log.Info("approver push subscription gone", "device", deviceID)
	default:
		s.opts.Log.Warn("approver push failed", "device", deviceID, "error", err)
	}
}

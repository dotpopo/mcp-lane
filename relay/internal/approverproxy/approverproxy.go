// Package approverproxy carries the approver API (D43, V-T4) through the
// relay: a phone the user paired talks to <relay>/v1/approver/*, and each
// call is forwarded into the tunnel of the Companion that minted the
// phone's credential.
//
// The relay keeps no table for this. A Companion behind a relay prefixes
// every pairing code and device id it hands out with its own relay device
// id and a dot, so the routing key is read from the request itself: from
// the code inside a claim, from the Authorization header of everything
// after. Whether the credential is any good is not decided here — the
// Companion verifies the signature, the nonce, the clock and its own
// device list, exactly as it does without a relay. Nothing in the request
// is read beyond what routing needs, nothing is held, and a Companion that
// is offline answers 503 at once: an approval is never queued.
package approverproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/dotpopo/mcp-lane/shared/tunnel"
)

// maxBody bounds an approver API request: a claim carries two public keys
// and a name, an answer a handle and a verdict. The Companion refuses more
// too; refusing here keeps the tunnel from carrying it.
const maxBody = 64 << 10

// scheme is the Authorization scheme of a signed device request, as the
// Companion's approver package defines it.
const scheme = "Fylane-Approver"

// Resolver turns the routing key found in a request into the identity of
// the tunnel to forward it through. It reports false when the key names
// nothing this relay would route to.
type Resolver func(key string) (deviceID string, ok bool)

// ByDeviceID routes to the Companion whose relay device id the key is: the
// OAuth relay, where each Companion holds its own tunnel.
func ByDeviceID(key string) (string, bool) { return key, key != "" }

// ToLegacy routes everything to the one shared-token Companion, whatever
// the key says: the self-hosted relay has only one tunnel to choose.
func ToLegacy(string) (string, bool) { return tunnel.LegacyIdentity, true }

// Handler serves /v1/approver/ by forwarding into the tunnel resolve picks.
// The page itself is not served here: mount approverpage.Routes beside it.
func Handler(ts *tunnel.Server, resolve Resolver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_body")
			return
		}
		if len(body) > maxBody {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
			return
		}
		key, status, code := routingKey(r, body)
		if code != "" {
			writeError(w, status, code)
			return
		}
		deviceID, ok := resolve(key)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "companion is offline; start it and retry")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		ts.ForwardAny(w, r, deviceID)
	})
}

// routingKey reads the key out of the one place each request carries it.
// A missing or unshaped key is refused with the status and code the
// Companion itself would give for a credential it cannot place, so the page
// shows the same state either way.
func routingKey(r *http.Request, body []byte) (key string, status int, code string) {
	if r.URL.Path == "/v1/approver/claim" {
		var req struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return "", http.StatusBadRequest, "invalid_request"
		}
		key, _, ok := strings.Cut(req.Code, ".")
		if !ok || key == "" {
			return "", http.StatusNotFound, "bad_code"
		}
		return key, 0, ""
	}
	header, ok := strings.CutPrefix(r.Header.Get("Authorization"), scheme+" ")
	if !ok {
		return "", http.StatusUnauthorized, "unauthorized"
	}
	id, _, _ := strings.Cut(strings.TrimSpace(header), ":")
	key, _, ok = strings.Cut(id, ".")
	if !ok || key == "" {
		return "", http.StatusUnauthorized, "unauthorized"
	}
	return key, 0, ""
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}

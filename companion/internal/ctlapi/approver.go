package ctlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dotpopo/mcp-lane/companion/internal/approver"
)

// ApproverControl is the settings page's view of the approver devices: who is
// paired, how to pair one more, and the withdraw. Implemented by
// *approver.Service. Pairing codes are minted here and nowhere reachable
// from the network, for the reason directsrv gives for platform pairing.
type ApproverControl interface {
	Pair() (approver.Pairing, error)
	Devices(ctx context.Context) ([]approver.Device, error)
	Revoke(ctx context.Context, id string) error
	Recent() []approver.Answer
}

// approverDoc is what the settings page renders.
type approverDoc struct {
	// Available is false when no public surface exists to pair through
	// (relay mode, until V-T4). The section then explains rather than offers.
	Available bool              `json:"available"`
	Devices   []approver.Device `json:"devices"`
	// Recent lets the desktop say "answered on <device>" for a prompt it
	// watched disappear.
	Recent []approver.Answer `json:"recent"`
}

func (s *Server) handleApprover(w http.ResponseWriter, r *http.Request) {
	doc := approverDoc{Devices: []approver.Device{}, Recent: []approver.Answer{}}
	if s.Approver == nil {
		writeJSON(w, doc)
		return
	}
	devices, err := s.Approver.Devices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	doc.Available = true
	if devices != nil {
		doc.Devices = devices
	}
	if recent := s.Approver.Recent(); recent != nil {
		doc.Recent = recent
	}
	writeJSON(w, doc)
}

// handleApproverPair mints the code the desktop shows as a QR code.
func (s *Server) handleApproverPair(w http.ResponseWriter, _ *http.Request) {
	if s.Approver == nil {
		http.Error(w, "approver devices need this machine's own public address", http.StatusConflict)
		return
	}
	p, err := s.Approver.Pair()
	if errors.Is(err, approver.ErrNoPublicURL) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"code": p.Code, "url": p.URL, "expires_in_seconds": int(p.ExpiresIn.Seconds())})
}

func (s *Server) handleApproverRevoke(w http.ResponseWriter, r *http.Request) {
	if s.Approver == nil {
		http.Error(w, "approver devices are not available", http.StatusConflict)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := s.Approver.Revoke(r.Context(), req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.handleApprover(w, r)
}

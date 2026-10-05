package ctlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/approval"
	"github.com/dotpopo/mcp-lane/companion/internal/approver"
	"github.com/dotpopo/mcp-lane/companion/internal/store"
	"github.com/dotpopo/mcp-lane/companion/internal/workspace"
)

// fakeApprover stands in for the service: the endpoints are wiring, and the
// wiring is what this file checks.
type fakeApprover struct {
	published bool
	devices   []approver.Device
	revoked   []string
}

func (f *fakeApprover) Pair() (approver.Pairing, error) {
	if !f.published {
		return approver.Pairing{}, approver.ErrNoPublicURL
	}
	return approver.Pairing{Code: "c0de", URL: "https://core.example/approver#c0de", ExpiresIn: 10 * time.Minute}, nil
}

func (f *fakeApprover) Devices(context.Context) ([]approver.Device, error) { return f.devices, nil }

func (f *fakeApprover) Revoke(_ context.Context, id string) error {
	for i, d := range f.devices {
		if d.ID == id {
			f.devices = append(f.devices[:i], f.devices[i+1:]...)
			f.revoked = append(f.revoked, id)
			return nil
		}
	}
	return errors.New("no such device")
}

func (f *fakeApprover) Recent() []approver.Answer {
	return []approver.Answer{{ChangeSetID: "cs_9", DeviceID: "apr_1", DeviceName: "Pixel", Approved: true}}
}

func approverFixture(t *testing.T, ctl ApproverControl) *fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "fylane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	manager, err := workspace.NewManager(st, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := approval.New(approval.ModeSafe, approval.DefaultBudgets(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Manager: manager, Store: st, Approvals: svc, Approver: ctl}
	addr, err := srv.Start(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{addr: addr, token: srv.token, svc: svc, manager: manager, st: st, srv: srv, dataDir: dataDir}
}

func TestApproverDevicesAreListedPairedAndWithdrawn(t *testing.T) {
	fake := &fakeApprover{devices: []approver.Device{{ID: "apr_1", Name: "Pixel"}}}
	f := approverFixture(t, fake)

	resp, raw := f.call(t, "POST", "/v1/approver/pair", f.token, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("pairing with no public address: %d %s", resp.StatusCode, raw)
	}
	fake.published = true
	resp, raw = f.call(t, "POST", "/v1/approver/pair", f.token, nil)
	var pairing struct {
		Code      string `json:"code"`
		URL       string `json:"url"`
		ExpiresIn int    `json:"expires_in_seconds"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &pairing) != nil ||
		pairing.Code != "c0de" || !strings.HasSuffix(pairing.URL, "#c0de") || pairing.ExpiresIn != 600 {
		t.Fatalf("pair: %d %s", resp.StatusCode, raw)
	}

	resp, raw = f.call(t, "GET", "/v1/approver", f.token, nil)
	var doc approverDoc
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &doc) != nil {
		t.Fatalf("list: %d %s", resp.StatusCode, raw)
	}
	if !doc.Available || len(doc.Devices) != 1 || doc.Devices[0].Name != "Pixel" ||
		len(doc.Recent) != 1 || doc.Recent[0].DeviceName != "Pixel" {
		t.Fatalf("doc = %+v", doc)
	}

	resp, raw = f.call(t, "POST", "/v1/approver/revoke", f.token, map[string]string{"id": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("revoke without an id: %d", resp.StatusCode)
	}
	resp, raw = f.call(t, "POST", "/v1/approver/revoke", f.token, map[string]string{"id": "apr_1"})
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &doc) != nil || len(doc.Devices) != 0 {
		t.Fatalf("revoke: %d %s", resp.StatusCode, raw)
	}
	if len(fake.revoked) != 1 || fake.revoked[0] != "apr_1" {
		t.Fatalf("the service was not told: %v", fake.revoked)
	}
	if resp, _ := f.call(t, "GET", "/v1/approver", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("the approver endpoints must sit behind the control token like everything else")
	}
}

func TestApproverSectionSaysUnavailableWithoutAService(t *testing.T) {
	f := approverFixture(t, nil)
	resp, raw := f.call(t, "GET", "/v1/approver", f.token, nil)
	var doc approverDoc
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &doc) != nil || doc.Available ||
		doc.Devices == nil || doc.Recent == nil {
		t.Fatalf("unavailable doc: %d %s", resp.StatusCode, raw)
	}
	if resp, _ := f.call(t, "POST", "/v1/approver/pair", f.token, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("pairing without a service: %d", resp.StatusCode)
	}
}

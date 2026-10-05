package app

import (
	"github.com/dotpopo/mcp-lane/companion/internal/approval"
	"github.com/dotpopo/mcp-lane/companion/internal/approver"
	"github.com/dotpopo/mcp-lane/companion/internal/ctlapi"
	"github.com/dotpopo/mcp-lane/companion/internal/devicecred"
	"github.com/dotpopo/mcp-lane/companion/internal/store"
	"github.com/dotpopo/mcp-lane/companion/internal/webpush"
)

// approverDevices builds the approver surface: prompts and decisions come
// from the same approval service the desktop uses, the view is the
// desktop's own, and both keys live in the OS keychain. publicURL is where
// a phone reaches this machine — its own address in direct mode, the
// relay's in relay mode, where routeKey is this machine's relay device id.
// Without a push key the surface still works; the phone just has to be
// open.
func (a *App) approverDevices(st *store.Store, approvals *approval.Service, publicURL func() string, routeKey string) (*approver.Service, error) {
	signer, err := devicecred.ApproverSigningKey()
	if err != nil {
		return nil, err
	}
	opts := approver.Options{
		Store:     st,
		Pending:   approvals.Pending,
		Resolve:   approvals.Resolve,
		View:      func(p *approval.Pending) any { return ctlapi.ApprovalView(p) },
		Signer:    signer,
		PublicURL: publicURL,
		RouteKey:  routeKey,
		Log:       a.log,
	}
	if key, err := devicecred.ApproverPushKey(); err != nil {
		a.log.Warn("approver push unavailable", "error", err)
	} else if sender, err := webpush.NewSender(key); err != nil {
		a.log.Warn("approver push unavailable", "error", err)
	} else {
		opts.Push = sender
	}
	return approver.New(opts)
}

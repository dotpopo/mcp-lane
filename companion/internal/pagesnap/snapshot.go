package pagesnap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dotpopo/mcp-lane/companion/internal/cmdexec"
	"github.com/dotpopo/mcp-lane/companion/internal/urlfetch"
)

const (
	// DefaultWidth and DefaultHeight are the viewport when the caller names
	// none: a laptop screen, legible after a platform downscales it.
	DefaultWidth  = 1280
	DefaultHeight = 800
	MinWidth      = 320
	MaxWidth      = 1920
	MinHeight     = 240
	MaxHeight     = 1200
	// MaxFullPageHeight caps a full-page picture; past it the page is cut.
	MaxFullPageHeight = 6000

	// takeTimeout bounds one snapshot end to end, browser start included.
	takeTimeout = 45 * time.Second
	// settleBudget is how long to wait for the page to go quiet after it
	// loads; a page that keeps polling is taken when it runs out.
	settleBudget = 15 * time.Second
	// quietFor is how long no request may be outstanding to call it settled.
	quietFor = 500 * time.Millisecond

	maxPageErrors   = 10
	maxPageErrorLen = 300
	maxTitleLen     = 200
)

// jpegQualities is tried in order until the picture fits the byte budget.
var jpegQualities = []int{80, 65, 50, 35}

var (
	// ErrNothingListening means no process holds the port.
	ErrNothingListening = errors.New("nothing is listening on this port")
	// ErrNotThisWorkspace means a process holds the port, but no command
	// running in this workspace started it.
	ErrNotThisWorkspace = errors.New("the process on this port was not started by a command running in this workspace")
	// ErrTooLarge means the picture does not fit the budget at any quality.
	ErrTooLarge = errors.New("the picture is too large to send even at the lowest quality")
)

// Request is one picture to take.
type Request struct {
	WorkspaceID string
	Port        int
	// Path is the page path, starting with "/", with any query.
	Path          string
	Width, Height int
	FullPage      bool
	// Network is the workspace's own setting: whether the page may reach
	// public addresses. Loopback and private addresses are decided apart
	// from it.
	Network bool
	// MaxBytes is the most the JPEG may weigh.
	MaxBytes int
}

// Shot is what was taken.
type Shot struct {
	JPEG          []byte
	Quality       int
	Width, Height int
	URL           string
	// HTTPStatus is the status of the page itself; 0 if none arrived.
	HTTPStatus int
	Title      string
	// Settled is false when the page was still loading at the deadline.
	Settled bool
	// Blocked names the destinations the page tried and was refused.
	Blocked []string
	// Errors are uncaught exceptions and console errors, in order, capped.
	Errors []string
}

// Snapshotter takes pictures with one installed browser.
type Snapshotter struct {
	// Browser is the executable, from FindBrowser.
	Browser string
	// Owns reports whether pid was started by a command running in the
	// workspace; *cmdexec.Runner.Owns.
	Owns func(workspaceID string, pid int) bool
	// Listeners names the processes on a port. Nil uses ListenerPIDs.
	Listeners func(ctx context.Context, port int) ([]int, error)
	// TempDir is where browser profiles are made. Empty uses the system's.
	TempDir string
}

// PageURL is the address a request opens, or an error for a path that is not
// a path on the page's own origin.
func PageURL(port int, path string) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("port %d is not a port", port)
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\\r\n\t") {
		return "", fmt.Errorf("path must start with a single / and name a page on the dev server, like /login")
	}
	ref, err := url.Parse(path)
	if err != nil || ref.Scheme != "" || ref.Host != "" {
		return "", fmt.Errorf("path must be a path on the dev server, not an address")
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", strconv.Itoa(port)),
		Path: ref.Path, RawPath: ref.RawPath, RawQuery: ref.RawQuery}
	return u.String(), nil
}

// Owner checks that a port belongs to the workspace before anything starts.
func (s *Snapshotter) Owner(ctx context.Context, workspaceID string, port int) error {
	return newOwnership(s, workspaceID).check(ctx, port)
}

// Take opens the page and returns its picture.
func (s *Snapshotter) Take(ctx context.Context, req Request) (*Shot, error) {
	pageURL, err := PageURL(req.Port, req.Path)
	if err != nil {
		return nil, err
	}
	width := clamp(req.Width, DefaultWidth, MinWidth, MaxWidth)
	height := clamp(req.Height, DefaultHeight, MinHeight, MaxHeight)

	ctx, cancel := context.WithTimeout(ctx, takeTimeout)
	defer cancel()

	owned := newOwnership(s, req.WorkspaceID)
	if err := owned.check(ctx, req.Port); err != nil {
		return nil, err
	}

	px, err := startProxy(func(ip net.IP, port int) error {
		switch {
		case ip.IsLoopback():
			if owned.check(ctx, port) != nil {
				return errBlocked
			}
			return nil
		case urlfetch.RejectNonPublic(ip) != nil:
			return errBlocked
		case !req.Network:
			return errBlocked
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("starting the snapshot proxy: %w", err)
	}
	defer px.close()

	profile, err := os.MkdirTemp(s.TempDir, "fylane-snapshot-")
	if err != nil {
		return nil, fmt.Errorf("making a browser profile: %w", err)
	}
	defer removeProfile(profile)

	b, err := launch(ctx, s.Browser, profile, px.addr())
	if err != nil {
		return nil, err
	}
	defer b.stop()

	page := newPageWatch()
	conn, err := dialCDP(ctx, b.wsURL, page.event)
	if err != nil {
		return nil, err
	}
	defer conn.close()
	defer func() {
		bye, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn.call(bye, "", "Browser.close", nil, nil)
	}()

	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := conn.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return nil, err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := conn.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		return nil, err
	}
	sid := attached.SessionID
	page.session(sid)
	for _, m := range []string{"Page.enable", "Network.enable", "Runtime.enable"} {
		if err := conn.call(ctx, sid, m, nil, nil); err != nil {
			return nil, err
		}
	}
	if err := conn.call(ctx, sid, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false,
	}, nil); err != nil {
		return nil, err
	}

	var nav struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := conn.call(ctx, sid, "Page.navigate", map[string]any{"url": pageURL}, &nav); err != nil {
		return nil, err
	}
	if nav.ErrorText != "" {
		return nil, fmt.Errorf("the page did not load: %s", nav.ErrorText)
	}
	settled := page.waitSettled(ctx, settleBudget)

	shot := &Shot{URL: pageURL, Width: width, Height: height, Settled: settled}
	shot.HTTPStatus, shot.Errors = page.result(nav.LoaderID)

	var title struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if conn.call(ctx, sid, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}, &title) == nil {
		shot.Title = cut(title.Result.Value, maxTitleLen)
	}

	params := map[string]any{"format": "jpeg"}
	if req.FullPage {
		var metrics struct {
			CSSContentSize struct {
				Height float64 `json:"height"`
			} `json:"cssContentSize"`
		}
		if err := conn.call(ctx, sid, "Page.getLayoutMetrics", nil, &metrics); err != nil {
			return nil, err
		}
		shot.Height = min(max(int(math.Ceil(metrics.CSSContentSize.Height)), height), MaxFullPageHeight)
		params["captureBeyondViewport"] = true
		params["clip"] = map[string]any{"x": 0, "y": 0, "width": width, "height": shot.Height, "scale": 1}
	}
	for _, q := range jpegQualities {
		params["quality"] = q
		var picture struct {
			Data []byte `json:"data"`
		}
		if err := conn.call(ctx, sid, "Page.captureScreenshot", params, &picture); err != nil {
			return nil, err
		}
		if req.MaxBytes <= 0 || len(picture.Data) <= req.MaxBytes {
			shot.JPEG, shot.Quality = picture.Data, q
			break
		}
	}
	if shot.JPEG == nil {
		return nil, ErrTooLarge
	}
	shot.Blocked = px.names()
	return shot, nil
}

func clamp(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	return min(max(v, lo), hi)
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ownership answers "is this port the workspace's" once per port for one
// snapshot; a page asks about the same few ports many times.
type ownership struct {
	s           *Snapshotter
	workspaceID string
	mu          sync.Mutex
	answers     map[int]error
}

func newOwnership(s *Snapshotter, workspaceID string) *ownership {
	return &ownership{s: s, workspaceID: workspaceID, answers: map[int]error{}}
}

func (o *ownership) check(ctx context.Context, port int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err, ok := o.answers[port]; ok {
		return err
	}
	err := o.lookup(ctx, port)
	o.answers[port] = err
	return err
}

func (o *ownership) lookup(ctx context.Context, port int) error {
	listeners := o.s.Listeners
	if listeners == nil {
		listeners = ListenerPIDs
	}
	pids, err := listeners(ctx, port)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return ErrNothingListening
	}
	for _, pid := range pids {
		if o.s.Owns != nil && o.s.Owns(o.workspaceID, pid) {
			return nil
		}
	}
	return ErrNotThisWorkspace
}

// browser is one running headless browser.
type browser struct {
	cmd    *osexec.Cmd
	group  *cmdexec.ProcessGroup
	wsURL  string
	exited chan struct{}
}

// launch starts the browser with its own profile and proxy and waits for it
// to say where its DevTools endpoint is.
func launch(ctx context.Context, exe, profile, proxyAddr string) (*browser, error) {
	cmd := osexec.Command(exe,
		"--headless=new",
		"--remote-debugging-port=0",
		"--user-data-dir="+profile,
		"--proxy-server=http://"+proxyAddr,
		// Chrome sends loopback addresses around a proxy unless told not to;
		// without this the page could reach any local port directly.
		"--proxy-bypass-list=<-loopback>",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-sync",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--mute-audio",
		"--hide-scrollbars",
		"about:blank",
	)
	group := cmdexec.NewProcessGroup()
	group.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		group.Close()
		return nil, fmt.Errorf("starting the browser: %w", err)
	}
	b := &browser{cmd: cmd, group: group, exited: make(chan struct{})}
	if err := group.Adopt(cmd); err != nil {
		cmd.Process.Kill()
	}
	go func() {
		cmd.Wait()
		close(b.exited)
	}()

	portFile := filepath.Join(profile, "DevToolsActivePort")
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(portFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) == 2 {
				b.wsURL = "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1])
				return b, nil
			}
		}
		select {
		case <-tick.C:
		case <-b.exited:
			b.stop()
			return nil, errors.New("the browser exited before it was ready")
		case <-ctx.Done():
			b.stop()
			return nil, fmt.Errorf("the browser did not start: %w", ctx.Err())
		}
	}
}

// stop takes the browser's whole process tree down and waits for it.
func (b *browser) stop() {
	select {
	case <-b.exited:
	default:
		b.group.Kill(b.cmd)
		select {
		case <-b.exited:
		case <-time.After(10 * time.Second):
		}
	}
	b.group.Close()
}

// removeProfile deletes a profile directory. A browser's helpers can still be
// letting go of files for a moment after the main process exits, so it tries
// a few times before giving up.
func removeProfile(dir string) {
	for i := 0; i < 20; i++ {
		if os.RemoveAll(dir) == nil {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pageWatch follows one page's events: outstanding requests, load, the
// document's status, and errors.
type pageWatch struct {
	mu        sync.Mutex
	sid       string
	loaded    bool
	inflight  map[string]bool
	changed   time.Time
	documents map[string]int
	errors    []string
}

func newPageWatch() *pageWatch {
	return &pageWatch{inflight: map[string]bool{}, documents: map[string]int{}, changed: time.Now()}
}

func (w *pageWatch) session(sid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sid = sid
}

func (w *pageWatch) event(m cdpMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if m.SessionID == "" || m.SessionID != w.sid {
		return
	}
	var p struct {
		RequestID string `json:"requestId"`
		LoaderID  string `json:"loaderId"`
		Type      string `json:"type"`
		Response  struct {
			Status int `json:"status"`
		} `json:"response"`
		ExceptionDetails struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
		Args []struct {
			Value       any    `json:"value"`
			Description string `json:"description"`
		} `json:"args"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	switch m.Method {
	case "Page.loadEventFired":
		w.loaded = true
		w.changed = time.Now()
	case "Network.requestWillBeSent":
		w.inflight[p.RequestID] = true
		w.changed = time.Now()
	case "Network.loadingFinished", "Network.loadingFailed":
		delete(w.inflight, p.RequestID)
		w.changed = time.Now()
	case "Network.responseReceived":
		if p.Type == "Document" {
			w.documents[p.RequestID] = p.Response.Status
		}
	case "Runtime.exceptionThrown":
		msg := p.ExceptionDetails.Exception.Description
		if msg == "" {
			msg = p.ExceptionDetails.Text
		}
		w.addError(msg)
	case "Runtime.consoleAPICalled":
		if p.Type != "error" {
			return
		}
		parts := make([]string, 0, len(p.Args))
		for _, a := range p.Args {
			switch {
			case a.Value != nil:
				parts = append(parts, fmt.Sprint(a.Value))
			case a.Description != "":
				parts = append(parts, a.Description)
			}
		}
		w.addError(strings.Join(parts, " "))
	}
}

func (w *pageWatch) addError(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" || len(w.errors) >= maxPageErrors {
		return
	}
	w.errors = append(w.errors, cut(msg, maxPageErrorLen))
}

// waitSettled waits until the page has loaded and no request has been
// outstanding for quietFor, or until budget runs out.
func (w *pageWatch) waitSettled(ctx context.Context, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		w.mu.Lock()
		quiet := w.loaded && len(w.inflight) == 0 && time.Since(w.changed) >= quietFor
		w.mu.Unlock()
		if quiet {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return false
		}
	}
}

// result is the document's status and the errors seen. The document request
// of a navigation carries the navigation's loader id as its request id.
func (w *pageWatch) result(loaderID string) (int, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.documents[loaderID], append([]string(nil), w.errors...)
}

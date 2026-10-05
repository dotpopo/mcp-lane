package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	reconnectInitialDelay = time.Second
	reconnectMaxDelay     = 30 * time.Second
	pingInterval          = 30 * time.Second
)

// DefaultMaxConcurrent caps how many tunneled requests the Companion serves
// at once when Client.MaxConcurrent is unset. Thirty-two concurrent tool
// calls already exceed any interactive session; beyond that the caller is
// either broken or hostile, and each admitted request holds a handler
// goroutine plus its buffers until it finishes.
const DefaultMaxConcurrent = 32

// Client is the Companion side of the tunnel: it dials the relay outbound
// (WSS), answers tunneled requests with Handler, and reconnects with backoff
// until ctx is done. The Companion never listens for inbound connections.
type Client struct {
	// RelayURL is the relay registration endpoint (ws:// or wss://).
	RelayURL string
	// Token authenticates the tunnel (FYLANE_TUNNEL_TOKEN).
	Token string
	// Handler serves the tunneled requests, e.g. the local MCP handler.
	Handler http.Handler
	// EagerSSE commits a "200 text/event-stream" response header for any
	// request still headerless after one keepalive interval. The MCP SDK
	// commits to SSE as soon as it accepts a tool call but flushes headers
	// only with the first event — minutes away when a local approval is
	// pending — which would let intermediary proxies time the call out.
	// Enable when Handler is an MCP Streamable HTTP handler. It applies to
	// requests for /mcp only: any other tunneled surface (the approver
	// inbox holds a long-poll open for longer than the interval) answers
	// with its own headers in its own time.
	EagerSSE bool

	// MaxConcurrent caps how many tunneled requests are served at once.
	// Zero or negative means DefaultMaxConcurrent. A request arriving past
	// the cap is refused at once with a 503 carrying a JSON error (see
	// answerOverloaded) — never queued without bound and never dropped
	// silently, so the caller can retry or shed load instead of waiting
	// behind handlers that may block on a human approval.
	MaxConcurrent int

	// connected reflects whether a relay connection is currently up; read
	// through Connected() (e.g. by the local status API).
	connected atomic.Bool
}

// Connected reports whether the tunnel currently holds a live relay
// connection.
func (c *Client) Connected() bool { return c.connected.Load() }

// Run connects and serves until ctx is canceled. It only returns early on
// configuration errors; network failures trigger reconnection.
func (c *Client) Run(ctx context.Context) error {
	if c.RelayURL == "" || c.Token == "" || c.Handler == nil {
		return errors.New("tunnel client needs RelayURL, Token, and Handler")
	}
	delay := reconnectInitialDelay
	for {
		wasConnected := c.serveSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		// A session that actually established resets the backoff; only
		// consecutive failed dials keep growing it.
		if wasConnected {
			delay = reconnectInitialDelay
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// serveSession runs one connection attempt and reports whether it got as far
// as an established tunnel (used to reset the reconnect backoff).
func (c *Client) serveSession(ctx context.Context) bool {
	connected, err := c.serveOnce(ctx)
	if ctx.Err() == nil {
		log.Printf("tunnel: connection lost (%v); reconnecting", err)
	}
	return connected
}

func (c *Client) serveOnce(ctx context.Context) (bool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ws, _, err := websocket.Dial(dialCtx, c.RelayURL, &websocket.DialOptions{
		HTTPHeader: http.Header{AuthHeader: {"Bearer " + c.Token}},
	})
	cancel()
	if err != nil {
		return false, err
	}
	ws.SetReadLimit(MaxFrameBytes)
	defer ws.Close(websocket.StatusNormalClosure, "")
	log.Printf("tunnel: connected to relay")
	c.connected.Store(true)
	defer c.connected.Store(false)

	connCtx, stop := context.WithCancel(ctx)
	defer stop()

	// Liveness probe: a dead TCP path is otherwise only noticed on the next
	// forwarded request.
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
				pingCtx, cancel := context.WithTimeout(connCtx, 10*time.Second)
				err := ws.Ping(pingCtx)
				cancel()
				if err != nil {
					stop()
					return
				}
			}
		}
	}()

	var writeMu sync.Mutex
	sem := make(chan struct{}, c.maxConcurrent())
	for {
		_, data, err := ws.Read(connCtx)
		if err != nil {
			return true, err
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil || f.Type != FrameRequest {
			continue
		}
		select {
		case sem <- struct{}{}:
			go func(f *Frame) {
				defer func() { <-sem }()
				c.answer(connCtx, ws, &writeMu, f)
			}(&f)
		default:
			// Past the cap: refuse now with a 503 the relay forwards
			// verbatim, so the caller sees a retryable refusal instead of
			// queueing behind an unbounded backlog. Its own goroutine
			// keeps the read loop flowing; the reply is three small
			// frames and holds no handler resources.
			go c.answerOverloaded(connCtx, ws, &writeMu, f.ID)
		}
	}
}

// maxConcurrent returns the effective per-connection request cap.
func (c *Client) maxConcurrent() int {
	if c.MaxConcurrent > 0 {
		return c.MaxConcurrent
	}
	return DefaultMaxConcurrent
}

// answerOverloaded refuses one request that arrived past MaxConcurrent. The
// protocol has no dedicated "busy" frame, so the refusal uses the closest
// thing it has: an ordinary 503 response (header, JSON body, end marker),
// which the relay forwards to the caller verbatim. A 503 with this body IS
// the overload signal — documented here so both ends read it the same way.
func (c *Client) answerOverloaded(ctx context.Context, ws *websocket.Conn, writeMu *sync.Mutex, id uint64) {
	frames := []*Frame{
		{Type: FrameResponseHeader, ID: id, Status: http.StatusServiceUnavailable,
			Header: http.Header{"Content-Type": {"application/json"}}},
		{Type: FrameResponseChunk, ID: id, Body: []byte(`{"error":"companion overloaded; retry shortly"}`)},
		{Type: FrameResponseEnd, ID: id},
	}
	for _, f := range frames {
		data, err := json.Marshal(f)
		if err != nil {
			log.Printf("tunnel: failed to encode overload refusal: %v", err)
			return
		}
		writeMu.Lock()
		writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = ws.Write(writeCtx, websocket.MessageText, data)
		cancel()
		writeMu.Unlock()
		if err != nil {
			log.Printf("tunnel: failed to send overload refusal: %v", err)
			return
		}
	}
}

// sseKeepaliveInterval is how often an idle SSE response gets a comment
// heartbeat so intermediary proxies (e.g. Cloudflare's ~100s idle budget) do
// not cut a pending tool call. SSE comments are ignored by clients.
// Variable for tests.
var sseKeepaliveInterval = 15 * time.Second

// answer serves one tunneled request, streaming the response back as
// header/chunk/end frames while the handler runs.
func (c *Client) answer(ctx context.Context, ws *websocket.Conn, writeMu *sync.Mutex, f *Frame) {
	rec := &streamRecorder{
		ctx:     ctx,
		ws:      ws,
		writeMu: writeMu,
		id:      f.ID,
		header:  make(http.Header),
	}
	req, err := buildRequest(ctx, f)
	if err != nil {
		rec.WriteHeader(http.StatusBadRequest)
	} else {
		rec.eagerSSE = c.EagerSSE && req.URL.Path == "/mcp"
		keepaliveDone := make(chan struct{})
		go rec.keepaliveLoop(keepaliveDone)
		c.Handler.ServeHTTP(rec, req)
		close(keepaliveDone)
	}
	rec.finish()
}

func buildRequest(ctx context.Context, f *Frame) (*http.Request, error) {
	u, err := url.ParseRequestURI(f.URL)
	if err != nil {
		return nil, err
	}
	header := f.Header
	if header == nil {
		header = make(http.Header)
	}
	req := &http.Request{
		Method:        f.Method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(f.Body)),
		ContentLength: int64(len(f.Body)),
		Host:          "companion.fylane.local",
		RemoteAddr:    "tunnel:0",
		RequestURI:    f.URL,
	}
	return req.WithContext(ctx), nil
}

// streamRecorder is the http.ResponseWriter handed to the local handler. It
// forwards the response as frames while the handler runs: WriteHeader emits
// a header frame, every Write emits chunk frames, finish emits the end
// frame. A keepalive loop injects SSE comment heartbeats at event boundaries
// while the handler is silent.
type streamRecorder struct {
	ctx     context.Context
	ws      *websocket.Conn
	writeMu *sync.Mutex
	id      uint64

	eagerSSE bool

	mu          sync.Mutex
	header      http.Header
	wroteHeader bool
	status      int
	broken      bool
	isSSE       bool
	// atBoundary is true when nothing has been written yet or the last
	// bytes were "\n\n" — the only points where injecting an SSE comment
	// cannot corrupt a partially written event.
	atBoundary bool
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeHeaderLocked(status)
}

func (r *streamRecorder) writeHeaderLocked(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.isSSE = strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream")
	r.atBoundary = true
	r.send(&Frame{Type: FrameResponseHeader, ID: r.id, Status: status, Header: r.header.Clone()})
}

func (r *streamRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.wroteHeader {
		r.writeHeaderLocked(http.StatusOK)
	}
	if r.broken {
		return 0, errors.New("tunnel response stream broken")
	}
	for off := 0; off < len(p); off += MaxChunkBytes {
		end := min(off+MaxChunkBytes, len(p))
		chunk := make([]byte, end-off)
		copy(chunk, p[off:end])
		if err := r.send(&Frame{Type: FrameResponseChunk, ID: r.id, Body: chunk}); err != nil {
			return off, err
		}
	}
	if len(p) > 0 {
		r.atBoundary = bytes.HasSuffix(p, []byte("\n\n"))
	}
	return len(p), nil
}

func (r *streamRecorder) Flush() {}

// finish emits the end-of-response frame after the handler returns.
func (r *streamRecorder) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.wroteHeader {
		r.writeHeaderLocked(http.StatusOK)
	}
	r.send(&Frame{Type: FrameResponseEnd, ID: r.id})
}

// keepaliveLoop injects `: keepalive` SSE comments while the handler is
// between events, so idle pending calls survive proxy idle timeouts.
func (r *streamRecorder) keepaliveLoop(done <-chan struct{}) {
	t := time.NewTicker(sseKeepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			// A call still headerless after a full interval is a hanging
			// SSE-mode tool call (see Client.EagerSSE). r.header belongs
			// to the handler goroutine and must not be read here, so a
			// fixed SSE header set is sent instead.
			if !r.wroteHeader && r.eagerSSE {
				h := http.Header{}
				h.Set("Content-Type", "text/event-stream")
				h.Set("Cache-Control", "no-cache, no-transform")
				r.wroteHeader = true
				r.status = http.StatusOK
				r.isSSE = true
				r.atBoundary = true
				r.send(&Frame{Type: FrameResponseHeader, ID: r.id, Status: http.StatusOK, Header: h})
			}
			if r.wroteHeader && r.isSSE && r.atBoundary && !r.broken {
				r.send(&Frame{Type: FrameResponseChunk, ID: r.id, Body: []byte(": keepalive\n\n")})
			}
			r.mu.Unlock()
		}
	}
}

// send marshals and writes one frame; callers hold r.mu.
func (r *streamRecorder) send(f *Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		r.broken = true
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	if err := r.ws.Write(writeCtx, websocket.MessageText, data); err != nil {
		r.broken = true
		log.Printf("tunnel: failed to send response frame: %v", err)
		return err
	}
	return nil
}

package pagesnap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPageURLStaysOnTheDevServer(t *testing.T) {
	for in, want := range map[string]string{
		"":                "http://localhost:5173/",
		"/":               "http://localhost:5173/",
		"/login":          "http://localhost:5173/login",
		"/a b?x=1&y=%20z": "http://localhost:5173/a%20b?x=1&y=%20z",
	} {
		got, err := PageURL(5173, in)
		if err != nil || got != want {
			t.Errorf("PageURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"login", "//evil.example/", "http://evil.example/", "/\\evil", "/a\r\nHost: x"} {
		if got, err := PageURL(5173, bad); err == nil {
			t.Errorf("PageURL(%q) = %q, want an error", bad, got)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := PageURL(port, "/"); err == nil {
			t.Errorf("port %d accepted", port)
		}
	}
}

func TestOwnershipIsTheWorkspacesProcessNotThePort(t *testing.T) {
	s := &Snapshotter{
		Owns: func(ws string, pid int) bool { return ws == "ws_a" && pid == 100 },
		Listeners: func(_ context.Context, port int) ([]int, error) {
			switch port {
			case 1:
				return []int{100}, nil
			case 2:
				return []int{200}, nil
			case 3:
				return []int{200, 100}, nil
			}
			return nil, nil
		},
	}
	ctx := context.Background()
	for _, tc := range []struct {
		ws   string
		port int
		want error
	}{
		{"ws_a", 1, nil},
		{"ws_b", 1, ErrNotThisWorkspace},
		{"ws_a", 2, ErrNotThisWorkspace},
		{"ws_a", 3, nil},
		{"ws_a", 4, ErrNothingListening},
	} {
		if err := s.Owner(ctx, tc.ws, tc.port); !errors.Is(err, tc.want) {
			t.Errorf("%s port %d: %v, want %v", tc.ws, tc.port, err, tc.want)
		}
	}
}

// TestASnapshotSeesThePageAndNothingElse runs a real browser when one is
// installed. The page tries three ways to reach a second local server — fetch,
// an image, a WebSocket — and the second server must never hear from it.
func TestASnapshotSeesThePageAndNothingElse(t *testing.T) {
	exe := FindBrowser()
	if exe == "" || testing.Short() {
		t.Skip("no Chromium-family browser installed")
	}
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits.Add(1)
	}))
	defer other.Close()
	otherPort := portOf(t, other)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<!doctype html><title>Fylane test</title>
<body style="margin:0;background:#1d4ed8"><h1 style="color:#fff">login</h1>
<script>
fetch("http://127.0.0.1:%[1]d/secret").catch(() => {});
new Image().src = "http://localhost:%[1]d/pixel";
try { new WebSocket("ws://127.0.0.1:%[1]d/ws"); } catch (e) {}
console.error("boom", 42);
</script>`, otherPort)
	}))
	defer page.Close()
	pagePort := portOf(t, page)

	profiles := t.TempDir()
	s := &Snapshotter{
		Browser: exe,
		TempDir: profiles,
		Owns:    func(ws string, pid int) bool { return ws == "ws_a" && pid == 1 },
		Listeners: func(_ context.Context, port int) ([]int, error) {
			switch port {
			case pagePort:
				return []int{1}, nil
			case otherPort:
				return []int{2}, nil
			}
			return nil, nil
		},
	}
	// A loaded CI runner can take longer than the snapshot's 45s budget to
	// bring the browser up — one Windows run passed in 30s, the next never
	// wrote its DevTools port in time. Retry once with a fresh deadline so
	// a slow start does not fail the suite; only browser-startup errors
	// retry, and a browser that cannot start at all still fails twice.
	var shot *Shot
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		attemptCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		shot, err = s.Take(attemptCtx, Request{WorkspaceID: "ws_a", Port: pagePort, Path: "/login", Width: 800, Height: 600, MaxBytes: 512 << 10})
		cancel()
		if err == nil || !strings.Contains(err.Error(), "browser") {
			break
		}
		t.Logf("snapshot attempt %d did not get a browser: %v", attempt+1, err)
	}
	if err != nil {
		t.Fatal(err)
	}

	img, err := jpeg.Decode(bytes.NewReader(shot.JPEG))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 800 || b.Dy() != 600 || shot.Width != 800 || shot.Height != 600 {
		t.Fatalf("picture %v, shot %dx%d", img.Bounds(), shot.Width, shot.Height)
	}
	r, g, bl, _ := img.At(400, 500).RGBA()
	if bl>>8 < 150 || r>>8 > 80 || g>>8 > 120 {
		t.Fatalf("the page background did not render: rgb(%d,%d,%d)", r>>8, g>>8, bl>>8)
	}
	if shot.Title != "Fylane test" || shot.HTTPStatus != 200 || !shot.Settled {
		t.Fatalf("title %q status %d settled %v", shot.Title, shot.HTTPStatus, shot.Settled)
	}
	if shot.URL != fmt.Sprintf("http://localhost:%d/login", pagePort) {
		t.Fatalf("url %q", shot.URL)
	}
	if len(shot.Errors) == 0 || !strings.Contains(shot.Errors[0], "boom 42") {
		t.Fatalf("errors %v", shot.Errors)
	}
	if n := otherHits.Load(); n != 0 {
		t.Fatalf("the page reached another local server %d times", n)
	}
	blocked := strings.Join(shot.Blocked, " ")
	if !strings.Contains(blocked, fmt.Sprint(otherPort)) {
		t.Fatalf("blocked %v does not name port %d", shot.Blocked, otherPort)
	}
	if left, _ := os.ReadDir(profiles); len(left) != 0 {
		t.Fatalf("browser profile left behind: %v", left)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := s.Take(ctx, Request{WorkspaceID: "ws_a", Port: otherPort, Path: "/"}); !errors.Is(err, ErrNotThisWorkspace) {
		t.Fatalf("another process's port: %v", err)
	}
}

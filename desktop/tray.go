package main

import (
	_ "embed"
	"fmt"
	goruntime "runtime"
	"time"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// trayIcon is a macOS template image: one colour plus alpha, 44px for the
// 22pt menu bar at 2x. The system tints it — black in a light menu bar, white
// in a dark one — which is why the file carries no colour of its own. The
// letter's counter is knocked out rather than filled, or the shape would read
// as a blob at this size.
//
//go:embed trayicon.png
var trayIcon []byte

// trayIconWindows is the same mark as a colour .ico: the Windows tray loads
// icons through LoadImage(IMAGE_ICON), which does not read PNG at all — a
// template PNG there yields a tooltip with nothing under it. Colour rather
// than a silhouette because Windows does not tint tray icons either.
//
//go:embed trayicon.ico
var trayIconWindows []byte

// trayPending is the "N requests waiting" row. Text only: the macOS template
// icon has no count dot matrix to draw into, and a second icon file for a
// badge would be a new asset for a number the words already carry.
var trayPending *systray.MenuItem

// startTray puts mcp-lane in the system tray using the spike-verified
// external-loop pattern and returns a stop function. The tray lives in the
// UI shell; the Core keeps serving even if the shell (and its tray) dies.
func startTray(app *App) func() {
	start, stop := systray.RunWithExternalLoop(func() {
		// Both arguments are the same file: the template is what macOS
		// wants, and on the platforms that ignore templates a one-colour
		// icon with alpha is still the right thing to draw.
		if goruntime.GOOS == "windows" {
			systray.SetIcon(trayIconWindows)
		} else {
			systray.SetTemplateIcon(trayIcon, trayIcon)
		}
		systray.SetTooltip("mcp-lane")
		open := systray.AddMenuItem("Open mcp-lane", "Show the mcp-lane window")
		trayPending = systray.AddMenuItem("No requests waiting", "Show the mcp-lane window")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit mcp-lane UI", "Close the UI shell (the Core keeps running)")
		go func() {
			for {
				select {
				case <-open.ClickedCh:
					app.showWindow()
				case <-trayPending.ClickedCh:
					app.showWindow()
				case <-quit.ClickedCh:
					if app.ctx != nil {
						runtime.Quit(app.ctx)
					}
				}
			}
		}()
		// The tray stands outside the window, so it reads the Core itself
		// rather than waiting for the frontend to tell it — a hidden window
		// still polls, and the count survives a closed one.
		go pollTrayPending(app)
	}, func() {})
	start()
	return stop
}

// pollTrayPending keeps the tray's pending row honest: how many approvals
// are held, re-read every few seconds. Unknown (the Core is not answering)
// keeps the last title rather than flashing one.
func pollTrayPending(app *App) {
	// Once at startup, so the row does not sit on its placeholder text for
	// a whole interval while the Core may already be answering.
	setTrayPending(app.pendingCount())
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for range tick.C {
		setTrayPending(app.pendingCount())
	}
}

func setTrayPending(n int) {
	if trayPending == nil || n < 0 {
		return
	}
	switch {
	case n == 0:
		trayPending.SetTitle("No requests waiting")
	case n == 1:
		trayPending.SetTitle("1 request waiting — open")
	default:
		trayPending.SetTitle(fmt.Sprintf("%d requests waiting — open", n))
	}
}

//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

const (
	// 3x the default UI font (Segoe UI 9pt). Set on the MainWindow so
	// child Label / TextEdit / PushButton inherit it.
	uiFontPointSize = 20

	askingWinW = 400
	askingWinH = 100
	resultWinW = 1100
	resultWinH = 750
)

// Last-known window position, shared between Asking and Result windows
// and persisted to disk so it survives restarts.
var (
	windowPosMu       sync.Mutex
	windowPosX        int
	windowPosY        int
	windowPosOK       bool
	windowPosLoadOnce sync.Once
)

const windowStateFile = "window_state.txt"

func windowStatePath() string {
	return filepath.Join(getExeDir(), windowStateFile)
}

func loadWindowPosFromDisk() {
	data, err := os.ReadFile(windowStatePath())
	if err != nil {
		return // first run, or unreadable — fall back to OS default
	}
	var x, y int
	if _, err := fmt.Sscanf(string(data), "%d %d", &x, &y); err != nil {
		return
	}
	windowPosX, windowPosY, windowPosOK = x, y, true
}

func savedWindowPos() (int, int, bool) {
	windowPosMu.Lock()
	defer windowPosMu.Unlock()
	windowPosLoadOnce.Do(loadWindowPosFromDisk)
	return windowPosX, windowPosY, windowPosOK
}

func saveWindowPos(x, y int) {
	windowPosMu.Lock()
	windowPosX = x
	windowPosY = y
	windowPosOK = true
	path := windowStatePath()
	payload := fmt.Sprintf("%d %d\n", x, y)
	windowPosMu.Unlock()

	// Best-effort persistence — a failure here just means the position
	// won't survive the next restart, which is preferable to crashing
	// the LLM UI flow.
	_ = os.WriteFile(path, []byte(payload), 0644)
}

// setTopmost pins a walk form above all non-topmost windows without
// stealing focus. Uses the same SetWindowPos flags as pinTop in main.go
// — crucially including SWP_SHOWWINDOW, since SetWindowPos on a hidden
// window doesn't reliably reorder the Z-order. The caller should Show()
// the window before calling setTopmost.
func setTopmost(w walk.Form) {
	procSetWindowPos.Call(
		uintptr(w.Handle()),
		HWND_TOPMOST,
		0, 0, 0, 0,
		SWP_NOSIZE|SWP_NOMOVE|SWP_NOACTIVATE|SWP_SHOWWINDOW,
	)
}

// attachPositionSaver records the window's bounds when it closes. Has
// to fire during Closing (not after Run() returns), because by the time
// Run() unblocks the HWND has been destroyed and Bounds() returns the
// zero Rectangle — which would persist (0, 0) and snap the next window
// to the top-left corner.
func attachPositionSaver(w walk.Form) {
	w.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		b := w.Bounds()
		if b.Width > 0 && b.Height > 0 {
			saveWindowPos(b.X, b.Y)
		}
	})
}

// keepTopmost re-asserts w as HWND_TOPMOST on a loop until stop is
// closed. Required because main.go's 3-second ticker keeps re-pinning
// the ER WorkFlow Panel as topmost, which would otherwise pop above
// our LLM dialogs (whichever window is most-recently SetWindowPos'd to
// topmost wins the Z-order among topmost windows). Fires immediately
// on entry rather than after a tick, so there's no initial gap where
// the dialog can be buried.
func keepTopmost(w walk.Form, stop <-chan struct{}) {
	go func() {
		for {
			hwnd := uintptr(w.Handle())
			if hwnd == 0 {
				return
			}
			procSetWindowPos.Call(
				hwnd,
				HWND_TOPMOST,
				0, 0, 0, 0,
				SWP_NOSIZE|SWP_NOMOVE|SWP_NOACTIVATE|SWP_SHOWWINDOW,
			)
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
	}()
}

// showLLMUI runs one LLM request end-to-end with UI feedback:
//
//  1. small topmost "Asking Claude..." window appears
//  2. API call fires on a worker goroutine
//  3. on completion the asking window closes
//  4. a topmost result window opens with the response + an OK button
//
// Each invocation runs on its own LockOSThread'd goroutine so multiple
// requests can be in flight at once. The Asking and Result windows
// share a session-wide last-known position.
func showLLMUI(text string, generateImpression bool) {
	go func() {
		runtime.LockOSThread()

		var askingWin *walk.MainWindow
		ask := decl.MainWindow{
			AssignTo: &askingWin,
			Title:    "mrgraise",
			Font:     decl.Font{PointSize: uiFontPointSize},
			MinSize:  decl.Size{Width: askingWinW, Height: askingWinH},
			MaxSize:  decl.Size{Width: askingWinW, Height: askingWinH},
			Size:     decl.Size{Width: askingWinW, Height: askingWinH},
			// Suppress the auto-show inside Create so we can position
			// and topmost-pin before the window appears on screen.
			Visible: false,
			Layout:  decl.VBox{},
			Children: []decl.Widget{
				decl.Label{
					Text:          "Asking Claude...",
					TextAlignment: decl.AlignCenter,
				},
			},
		}
		if x, y, ok := savedWindowPos(); ok {
			ask.Bounds = decl.Rectangle{X: x, Y: y, Width: askingWinW, Height: askingWinH}
		}
		if err := ask.Create(); err != nil {
			showError("UI error: " + err.Error())
			return
		}
		attachPositionSaver(askingWin)
		askingWin.Show()
		setTopmost(askingWin)
		askingStop := make(chan struct{})
		keepTopmost(askingWin, askingStop)

		apiDone := make(chan struct{})
		var (
			llmResult string
			llmErr    error
		)
		go func() {
			defer close(apiDone)
			cfg, err := getLLMConfig()
			if err != nil {
				llmErr = err
				return
			}
			llmResult, llmErr = runLLMQuery(cfg, text, generateImpression)
		}()

		go func() {
			<-apiDone
			askingWin.Synchronize(func() {
				askingWin.Close()
			})
		}()

		askingWin.Run()
		close(askingStop)

		// If the user closed the asking window before the API returned,
		// drop the in-flight request rather than popping a result window
		// they didn't ask to see.
		select {
		case <-apiDone:
		default:
			return
		}

		if llmErr != nil {
			showError("LLM error: " + llmErr.Error())
			return
		}

		var resultWin *walk.MainWindow
		res := decl.MainWindow{
			AssignTo: &resultWin,
			Title:    "mrgraise - Claude",
			Font:     decl.Font{PointSize: uiFontPointSize},
			MinSize:  decl.Size{Width: resultWinW, Height: resultWinH},
			MaxSize:  decl.Size{Width: resultWinW, Height: resultWinH},
			Size:     decl.Size{Width: resultWinW, Height: resultWinH},
			Visible:  false,
			Layout:   decl.VBox{},
			Children: []decl.Widget{
				decl.TextEdit{
					Text:     normalizeNewlines(llmResult),
					ReadOnly: true,
					VScroll:  true,
				},
				decl.PushButton{
					Text: "OK",
					OnClicked: func() {
						resultWin.Close()
					},
				},
			},
		}
		if x, y, ok := savedWindowPos(); ok {
			res.Bounds = decl.Rectangle{X: x, Y: y, Width: resultWinW, Height: resultWinH}
		}
		if err := res.Create(); err != nil {
			showError("UI error: " + err.Error())
			return
		}
		attachPositionSaver(resultWin)
		resultWin.Show()
		setTopmost(resultWin)
		resultStop := make(chan struct{})
		keepTopmost(resultWin, resultStop)

		resultWin.Run()
		close(resultStop)
	}()
}

// normalizeNewlines converts bare \n to \r\n so multiline text renders
// correctly in the Win32 Edit control behind walk.TextEdit.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

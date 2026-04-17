package main

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procFindWindowW  = user32.NewProc("FindWindowW")
	procIsIconic     = user32.NewProc("IsIconic")
	procShowWindow   = user32.NewProc("ShowWindow")
	procSetWindowPos = user32.NewProc("SetWindowPos")
)

const (
	WIN_TITLE  = "ER WorkFlow Panel"
	SW_RESTORE = 9

	// SetWindowPos Flags
	HWND_TOPMOST   = ^uintptr(0) // -1: Places window above all non-topmost windows
	SWP_NOSIZE     = 0x0001      // Retains current size
	SWP_NOMOVE     = 0x0002      // Retains current position
	SWP_NOACTIVATE = 0x0010      // Does NOT activate the window (no focus steal)
	SWP_SHOWWINDOW = 0x0040      // Displays the window
)

func main() {
	fmt.Printf("Watching for: %s\n", WIN_TITLE)
	fmt.Println("Pinned to 'Always on Top'.\n")
	fmt.Println("If you close this window, the program will quit, but it's ok to minimize it to the taskbar.\n")
	fmt.Println("To quit, press Ctrl-C, or click the [X] in the top right corner.\n")

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Convert Go string to UTF-16 pointer
		titlePtr, _ := syscall.UTF16PtrFromString(WIN_TITLE)

		// 1. Find the window handle (HWND)
		hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))

		if hwnd != 0 {
			// 2. Check if it's minimized; restore if necessary
			minimized, _, _ := procIsIconic.Call(hwnd)
			if minimized != 0 {
				procShowWindow.Call(hwnd, SW_RESTORE)
			}

			// 3. Set to Always on Top (HWND_TOPMOST)
			// and ensure focus isn't stolen (SWP_NOACTIVATE)
			procSetWindowPos.Call(
				hwnd,
				HWND_TOPMOST,
				0, 0, 0, 0,
				SWP_NOSIZE|SWP_NOMOVE|SWP_NOACTIVATE|SWP_SHOWWINDOW,
			)
		}
	}
}

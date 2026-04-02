package main

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procIsIconic            = user32.NewProc("IsIconic") // Checks if minimized
	procShowWindow          = user32.NewProc("ShowWindow")
)

const SW_RESTORE = 9
const WIN_TITLE = "ER WorkFlow Panel"

func main() {
	windowTitle := WIN_TITLE  // Change this to your target title
	fmt.Printf("Watching for: %s\n", windowTitle)
	fmt.Println("Click the [X] in the upper right to quit, or press Ctrl-C.\n")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Convert Go string to UTF-16 pointer
		titlePtr, _ := syscall.UTF16PtrFromString(windowTitle)

		// 1. Find the window handle (HWND)
		// FindWindowW(lpClassName, lpWindowName)
		hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))

		if hwnd != 0 {
			// fmt.Println("ER window appeared.")
			// 2. Check if it's minimized (Iconic)
			minimized, _, _ := procIsIconic.Call(hwnd)
			if minimized != 0 {
				// Restore it if it's minimized
				procShowWindow.Call(hwnd, SW_RESTORE)
			}

			// 3. Bring to front
			success, _, _ := procSetForegroundWindow.Call(hwnd)
			if success != 0 {
				// fmt.Println("Window raised!")
			}
		}
	}
}
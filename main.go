package main

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procIsIconic            = user32.NewProc("IsIconic")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

const (
	WIN_TITLE  = "ER WorkFlow Panel"
	SW_RESTORE = 9

	// SetWindowPos Flags
	HWND_TOPMOST   = ^uintptr(0) // -1: Places window above all non-topmost windows
	HWND_TOP       = 0           // Places window at top of the Z order (not topmost)
	SWP_NOSIZE     = 0x0001      // Retains current size
	SWP_NOMOVE     = 0x0002      // Retains current position
	SWP_NOACTIVATE = 0x0010      // Does NOT activate the window (no focus steal)
	SWP_SHOWWINDOW = 0x0040      // Displays the window

	WH_KEYBOARD_LL = 13
	WM_KEYDOWN     = 0x0100
	WM_SYSKEYDOWN  = 0x0104

	VK_F5 = 0x74
	VK_F6 = 0x75
	VK_F7 = 0x76
)

type kbdllhookstruct struct {
	vkCode      uint32
	scanCode    uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
}

// enumProc state — mutated only while EnumWindows is running on the
// main goroutine, so no synchronization is needed.
var (
	searchPrefix []uint16
	searchResult uintptr
)

func enumProc(hwnd uintptr, _ uintptr) uintptr {
	var buf [256]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if int(n) < len(searchPrefix) {
		return 1 // continue enumeration
	}
	for i, c := range searchPrefix {
		if buf[i] != c {
			return 1
		}
	}
	searchResult = hwnd
	return 0 // stop
}

var enumCallback = syscall.NewCallback(enumProc)

// findWindowByPrefix returns the first top-level window whose title
// starts with the given prefix, or 0 if none is found.
func findWindowByPrefix(prefix string) uintptr {
	utf16, _ := syscall.UTF16FromString(prefix)
	if len(utf16) > 0 && utf16[len(utf16)-1] == 0 {
		utf16 = utf16[:len(utf16)-1]
	}
	searchPrefix = utf16
	searchResult = 0
	procEnumWindows.Call(enumCallback, 0)
	return searchResult
}

func findWindowExact(title string) uintptr {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	return hwnd
}

func restoreIfMinimized(hwnd uintptr) {
	minimized, _, _ := procIsIconic.Call(hwnd)
	if minimized != 0 {
		procShowWindow.Call(hwnd, SW_RESTORE)
	}
}

// raiseWindow brings hwnd to the top of the Z order and activates it.
// Not pinned as topmost. One-shot.
func raiseWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	restoreIfMinimized(hwnd)
	procSetWindowPos.Call(
		hwnd,
		HWND_TOP,
		0, 0, 0, 0,
		SWP_NOSIZE|SWP_NOMOVE|SWP_SHOWWINDOW,
	)
	procSetForegroundWindow.Call(hwnd)
}

// pinTop asserts hwnd as always-on-top without activating it.
func pinTop(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	restoreIfMinimized(hwnd)
	procSetWindowPos.Call(
		hwnd,
		HWND_TOPMOST,
		0, 0, 0, 0,
		SWP_NOSIZE|SWP_NOMOVE|SWP_NOACTIVATE|SWP_SHOWWINDOW,
	)
}

var keyEvents = make(chan uint32, 8)

func keyboardHookProc(nCode uintptr, wParam uintptr, lParam uintptr) uintptr {
	if int32(nCode) == 0 && (wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN) {
		// Reinterpret lParam (a uintptr holding a pointer) as *kbdllhookstruct.
		// Going through &lParam avoids vet's unsafe.Pointer(uintptr) warning.
		k := *(**kbdllhookstruct)(unsafe.Pointer(&lParam))
		if k.vkCode == VK_F5 || k.vkCode == VK_F6 || k.vkCode == VK_F7 {
			select {
			case keyEvents <- k.vkCode:
			default:
			}
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return ret
}

var hookCallback = syscall.NewCallback(keyboardHookProc)

// runKeyboardHook installs a low-level keyboard hook and pumps messages
// so Windows can dispatch hook callbacks to this thread. The hook only
// observes keys — it does not swallow them.
func runKeyboardHook() {
	runtime.LockOSThread()
	h, _, err := procSetWindowsHookExW.Call(WH_KEYBOARD_LL, hookCallback, 0, 0)
	if h == 0 {
		fmt.Printf("failed to install keyboard hook: %v\n", err)
		return
	}
	var msg [64]byte // MSG struct; exact layout doesn't matter — only size does
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
	}
}

func main() {
	fmt.Printf("Pinning to top: %s\n", WIN_TITLE)
	fmt.Println("Hotkeys: \nF5 = Report Viewer, \nF6 = Order Viewer, \nF7 = Patient Record/Worklist")
	fmt.Println()
	fmt.Println("If you close this window, the program will quit, but it's ok to minimize it to the taskbar.")
	fmt.Println()
	fmt.Println("To quit, press Ctrl-C, or click the [X] in the top right corner.")
	fmt.Println()

	go runKeyboardHook()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pinTop(findWindowExact(WIN_TITLE))
		case vk := <-keyEvents:
			switch vk {
			case VK_F6:
				raiseWindow(findWindowByPrefix("Order Viewer:"))
			case VK_F5:
				raiseWindow(findWindowByPrefix("Report Viewer:"))
			case VK_F7:
				raiseWindow(findWindowByPrefix("Merge RealTime"))
				raiseWindow(findWindowByPrefix("RealTime"))
			}
		}
	}
}

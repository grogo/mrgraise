//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// debug is set by -d/--debug and gates the diagnostic prints in the F5
// cycle handler. Off by default.
var debug bool

func debugf(format string, args ...any) {
	if debug {
		fmt.Printf(format, args...)
	}
}

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
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
	procOpenClipboard       = user32.NewProc("OpenClipboard")
	procEmptyClipboard      = user32.NewProc("EmptyClipboard")
	procSetClipboardData    = user32.NewProc("SetClipboardData")
	procGetClipboardData    = user32.NewProc("GetClipboardData")
	procCloseClipboard      = user32.NewProc("CloseClipboard")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procMessageBeep         = user32.NewProc("MessageBeep")
	procKeybdEvent          = user32.NewProc("keybd_event")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procGlobalAlloc         = kernel32.NewProc("GlobalAlloc")
	procGlobalLock          = kernel32.NewProc("GlobalLock")
	procGlobalUnlock        = kernel32.NewProc("GlobalUnlock")
	procGlobalSize          = kernel32.NewProc("GlobalSize")
	procGlobalFree          = kernel32.NewProc("GlobalFree")
	procCreateMutexW        = kernel32.NewProc("CreateMutexW")
)

// CreateMutex sets this as the last error when the named mutex already
// exists (i.e., another instance of mrgraise owns it). The handle is
// still returned successfully in that case, so this is the value we
// actually have to check.
const ERROR_ALREADY_EXISTS syscall.Errno = 183

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
	WM_KEYUP       = 0x0101
	WM_SYSKEYDOWN  = 0x0104
	WM_SYSKEYUP    = 0x0105

	// kbdllhookstruct.flags bit set on events synthesized by SendInput /
	// keybd_event. Used to ignore our own injected keystrokes.
	LLKHF_INJECTED = 0x10

	VK_F5      = 0x74
	VK_F6      = 0x75
	VK_F8      = 0x77
	VK_CONTROL = 0x11
	VK_MENU    = 0x12 // Alt
	VK_C       = 0x43
	VK_S       = 0x53
	VK_V       = 0x56

	KEYEVENTF_KEYUP = 0x0002

	CF_UNICODETEXT = 13
	GMEM_MOVEABLE  = 0x0002

	MB_OK            = 0x00000000
	MB_ICONERROR     = 0x00000010
	MB_SETFOREGROUND = 0x00010000
	MB_TOPMOST       = 0x00040000
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
	searchPrefix  []uint16
	searchResults []uintptr
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
	searchResults = append(searchResults, hwnd)
	return 1 // continue — collect every match
}

var enumCallback = syscall.NewCallback(enumProc)

// findAllWindowsByPrefix returns every top-level window whose title
// starts with the given prefix, in EnumWindows order. Returns nil if
// none match.
func findAllWindowsByPrefix(prefix string) []uintptr {
	utf16, _ := syscall.UTF16FromString(prefix)
	if len(utf16) > 0 && utf16[len(utf16)-1] == 0 {
		utf16 = utf16[:len(utf16)-1]
	}
	searchPrefix = utf16
	searchResults = nil
	procEnumWindows.Call(enumCallback, 0)
	return searchResults
}

// findWindowByPrefix returns the first top-level window whose title
// starts with the given prefix, or 0 if none is found.
func findWindowByPrefix(prefix string) uintptr {
	all := findAllWindowsByPrefix(prefix)
	if len(all) == 0 {
		return 0
	}
	return all[0]
}

func findWindowExact(title string) uintptr {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	return hwnd
}

// getWindowText returns the title of hwnd as a Go string.
func getWindowText(hwnd uintptr) string {
	var buf [512]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// dumpAllTitles prints every non-empty top-level window title. Used
// to diagnose why a prefix search didn't match. Caller must check the
// debug flag.
func dumpAllTitles() {
	all := findAllWindowsByPrefix("")
	fmt.Printf("    -- %d top-level windows --\n", len(all))
	for _, hwnd := range all {
		t := getWindowText(hwnd)
		if t == "" {
			continue
		}
		fmt.Printf("      %#x %q\n", hwnd, t)
	}
}

// ACC field format: NN-TT-NN-NNNNNN (digits-letters-digits-digits).
var accRe = regexp.MustCompile(`^\d{2}-[A-Za-z]{2}-\d{2}-\d{6}$`)

// Service-date field format: MM/DD/YYYY (1- or 2-digit month/day).
var dateRe = regexp.MustCompile(`^\d{1,2}/\d{1,2}/\d{4}$`)

// parseOrderViewerTitle pulls the patient fields out of an Order Viewer
// title bar. Name/DOB/Loc/MRN are positional (fields 0..3 of the
// pipe-separated layout). ACC and service date are located by pattern
// because their positions vary; the scan starts at field 4 so DOB
// cannot be mistaken for the service date. Exam type sits next-to-last
// in a populated title. Any field that cannot be found is returned
// empty.
func parseOrderViewerTitle(title string) (name, dob, loc, mrn, date, acc, exam string) {
	parts := strings.Split(title, "|")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	if len(parts) > 0 {
		name = strings.TrimSpace(strings.TrimPrefix(parts[0], "Order Viewer:"))
	}
	if len(parts) > 1 {
		dob = parts[1]
	}
	if len(parts) > 2 {
		loc = parts[2]
	}
	if len(parts) > 3 {
		mrn = parts[3]
	}
	for i := 4; i < len(parts); i++ {
		p := parts[i]
		if acc == "" && accRe.MatchString(p) {
			acc = p
		}
		if date == "" && dateRe.MatchString(p) {
			date = p
		}
	}
	// Exam needs room for Name/DOB/Loc/MRN plus at least exam + trailing
	// modality, so require ≥6 fields before treating next-to-last as exam.
	if len(parts) >= 6 {
		exam = parts[len(parts)-2]
	}
	return
}

// setClipboardText places text on the Windows clipboard as
// CF_UNICODETEXT. On success the global memory block is owned by the
// clipboard and must not be freed here.
func setClipboardText(text string) error {
	utf16, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return fmt.Errorf("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()

	procEmptyClipboard.Call()

	hMem, _, _ := procGlobalAlloc.Call(GMEM_MOVEABLE, uintptr(len(utf16)*2))
	if hMem == 0 {
		return fmt.Errorf("GlobalAlloc failed")
	}
	ptr, _, _ := procGlobalLock.Call(hMem)
	if ptr == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("GlobalLock failed")
	}
	// Reinterpret ptr (a uintptr holding a pointer) as *uint16 via
	// &ptr — same pattern used for the kbdllhookstruct lParam above,
	// to avoid vet's unsafe.Pointer(uintptr) warning.
	dst := unsafe.Slice(*(**uint16)(unsafe.Pointer(&ptr)), len(utf16))
	copy(dst, utf16)
	procGlobalUnlock.Call(hMem)

	if r, _, _ := procSetClipboardData.Call(CF_UNICODETEXT, hMem); r == 0 {
		procGlobalFree.Call(hMem)
		return fmt.Errorf("SetClipboardData failed")
	}
	return nil
}

// getClipboardText reads CF_UNICODETEXT from the clipboard. Returns ""
// (no error) if there is no Unicode text on the clipboard.
func getClipboardText() (string, error) {
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return "", fmt.Errorf("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()

	hMem, _, _ := procGetClipboardData.Call(CF_UNICODETEXT)
	if hMem == 0 {
		return "", nil
	}
	ptr, _, _ := procGlobalLock.Call(hMem)
	if ptr == 0 {
		return "", fmt.Errorf("GlobalLock failed")
	}
	defer procGlobalUnlock.Call(hMem)

	size, _, _ := procGlobalSize.Call(hMem)
	if size == 0 {
		return "", nil
	}
	// Reinterpret ptr as *uint16; UTF16ToString stops at the first NUL,
	// so the over-large slice (from rounded-up GlobalAlloc block size)
	// is fine.
	buf := unsafe.Slice(*(**uint16)(unsafe.Pointer(&ptr)), int(size)/2)
	return syscall.UTF16ToString(buf), nil
}

// clearClipboard removes all formats from the clipboard.
func clearClipboard() error {
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return fmt.Errorf("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	return nil
}

// sendCtrlKey synthesizes Ctrl+<vk> as a global keystroke into whatever
// window currently has focus.
func sendCtrlKey(vk uintptr) {
	procKeybdEvent.Call(VK_CONTROL, 0, 0, 0)
	procKeybdEvent.Call(vk, 0, 0, 0)
	procKeybdEvent.Call(vk, 0, KEYEVENTF_KEYUP, 0)
	procKeybdEvent.Call(VK_CONTROL, 0, KEYEVENTF_KEYUP, 0)
}

// captureSelectionViaClipboard clears the clipboard, synthesizes Ctrl+C
// against the focused window, and returns whatever Unicode text landed
// on the clipboard. Empty string with nil error means nothing was
// selected. The caller's pre-existing clipboard contents are NOT
// preserved.
func captureSelectionViaClipboard() (string, error) {
	if err := clearClipboard(); err != nil {
		return "", err
	}
	sendCtrlKey(VK_C)
	time.Sleep(100 * time.Millisecond)
	return getClipboardText()
}

// releaseHotkeyModifiers injects key-up events for Alt and Ctrl so that
// the next synthesized Ctrl+C/V isn't confused by the modifiers still
// being physically held from a triggering Ctrl+Alt+X hotkey.
func releaseHotkeyModifiers() {
	procKeybdEvent.Call(VK_MENU, 0, KEYEVENTF_KEYUP, 0)
	procKeybdEvent.Call(VK_CONTROL, 0, KEYEVENTF_KEYUP, 0)
	time.Sleep(50 * time.Millisecond)
}

// renumberSelectionViaClipboard copies the focused window's current
// selection (Ctrl+C), runs it through the renumber pipeline, and pastes
// the result back (Ctrl+V). Does nothing if no text was selected or the
// selection was whitespace-only.
func renumberSelectionViaClipboard() {
	result, err := captureSelectionViaClipboard()
	if err != nil {
		showError("Clipboard error: " + err.Error() + "\n\nPlease select text first before running this shortcut.")
		return
	}
	if strings.TrimSpace(result) == "" {
		return
	}

	out := numberParagraphs(stripMarkdown(removeNumbering(result))) + "\n"
	if err := setClipboardText(out); err != nil {
		showError("Clipboard error: " + err.Error() + "\n\nPlease select text first before running this shortcut.")
		return
	}

	sendCtrlKey(VK_V)
}

// runLLMOnSelectionAsync captures the focused window's current selection
// and hands it off to showLLMUI, which runs the API call on its own
// LockOSThread'd goroutine while displaying Asking/Result windows. The
// topmost-pin ticker in the main loop is never blocked.
func runLLMOnSelectionAsync(generateImpression bool) {
	// User triggered this with Ctrl+Alt+S/C; release those modifiers so
	// the synthesized Ctrl+C below isn't seen as Ctrl+Alt+C.
	releaseHotkeyModifiers()

	// Validate config up front so missing prompts.ini / secrets.ini /
	// defaults.ini surfaces as a clean message instead of a downstream
	// walk error from inside showLLMUI.
	if _, err := getLLMConfig(); err != nil {
		msg := strings.TrimPrefix(err.Error(), "assertion failed: ")
		showError("Cannot load LLM config: " + msg + "\n\nmrgraise.exe needs defaults.ini, prompts.ini, and secrets.ini in the same directory as the executable.")
		return
	}

	text, err := captureSelectionViaClipboard()
	if err != nil {
		showError("Clipboard error: " + err.Error() + "\n\nPlease select text first before running this shortcut.")
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}

	showLLMUI(text, generateImpression)
}

// showError displays a Windows modal error dialog. Blocks until the
// user dismisses it. A watchdog goroutine re-asserts the dialog as
// HWND_TOPMOST every 300ms so it can't be hidden behind another
// topmost window (e.g., ER WorkFlow Panel, which the ticker keeps
// pinned).
func showError(msg string) {
	fmt.Println("Error:", msg)
	procMessageBeep.Call(MB_OK)

	// Run the dialog and its watchdog on a separate goroutine so the
	// caller (typically the main select loop) is not blocked until the
	// user dismisses the dialog. Multiple errors in quick succession
	// will stack multiple dialogs; that's acceptable.
	go func() {
		done := make(chan struct{})
		go func() {
			// Re-pin immediately on each iteration (no leading delay),
			// matching the keepTopmost pattern in ui.go: SetWindowPos
			// on a hidden window doesn't reliably reorder the Z-order,
			// so we need to fire as soon as the MessageBox HWND exists.
			// SWP_SHOWWINDOW is what actually moves a hidden topmost
			// window to the front of the topmost group.
			for {
				hwnd := findWindowExact("mrgraise")
				if hwnd != 0 {
					procSetWindowPos.Call(
						hwnd,
						HWND_TOPMOST,
						0, 0, 0, 0,
						SWP_NOSIZE|SWP_NOMOVE|SWP_NOACTIVATE|SWP_SHOWWINDOW,
					)
				}
				select {
				case <-done:
					return
				case <-time.After(150 * time.Millisecond):
				}
			}
		}()
		defer close(done)

		title, _ := syscall.UTF16PtrFromString("mrgraise")
		body, _ := syscall.UTF16PtrFromString(msg)
		procMessageBoxW.Call(
			0,
			uintptr(unsafe.Pointer(body)),
			uintptr(unsafe.Pointer(title)),
			MB_OK|MB_ICONERROR|MB_TOPMOST|MB_SETFOREGROUND,
		)
	}()
}

// copyOrderInfoToClipboard locates the Order Viewer window, pulls the
// patient fields from its title bar, and writes them to the clipboard
// as a tab-separated record (Name\tDOB\tLoc\tMRN\tDate\tACC\tExam).
// Also appends the record (prefixed with an ISO-8601 local timestamp)
// to the save file resolved by savedAccessionsPath. Shows a Windows
// error dialog if the window is not open or the clipboard call fails;
// file-write errors are logged to stdout only.
func copyOrderInfoToClipboard() {
	hwnd := findWindowByPrefix("Order Viewer:")
	if hwnd == 0 {
		showError("Warning: F6 copy failed because Order Viewer window was not found. Please open the Order Viewer window in Merge first.")
		return
	}
	name, dob, loc, mrn, date, acc, exam := parseOrderViewerTitle(getWindowText(hwnd))
	record := strings.Join([]string{name, dob, loc, mrn, date, acc, exam}, "\t")
	if err := setClipboardText(record); err != nil {
		showError("Clipboard error: " + err.Error())
		return
	}
	appendAccessionRecord(record)
}

// savedAccessionsPath returns the file path where each F6 copy is
// appended. Resolution order: [save] accessions_file in defaults.ini,
// then %USERPROFILE%\Documents\saved_accessions.txt.
func savedAccessionsPath() string {
	if iniFile, err := loadIniDiskOrEmbedded("defaults.ini", embeddedDefaultsIni); err == nil {
		if key, err := iniFile.Section("save").GetKey("accessions_file"); err == nil {
			if v := strings.TrimSpace(key.String()); v != "" {
				return v
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "saved_accessions.txt"
	}
	return filepath.Join(home, "Documents", "saved_accessions.txt")
}

// appendAccessionRecord appends one tab-separated record, preceded by
// an ISO-8601 local timestamp, as a new line to the save file. Errors
// are logged to stdout — the clipboard copy succeeds independently.
func appendAccessionRecord(record string) {
	path := savedAccessionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Printf("saved_accessions: cannot create %s: %v\n", filepath.Dir(path), err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Printf("saved_accessions: cannot open %s: %v\n", path, err)
		return
	}
	defer f.Close()
	line := time.Now().Format("2006-01-02T15:04:05") + "\t" + record + "\n"
	if _, err := f.WriteString(line); err != nil {
		fmt.Printf("saved_accessions: cannot write to %s: %v\n", path, err)
	}
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
	if int32(nCode) == 0 {
		// Reinterpret lParam (a uintptr holding a pointer) as *kbdllhookstruct.
		// Going through &lParam avoids vet's unsafe.Pointer(uintptr) warning.
		k := *(**kbdllhookstruct)(unsafe.Pointer(&lParam))
		// Ignore events we synthesized ourselves (sendCtrlKey, etc.) so
		// our own Ctrl+C/Ctrl+V injections can't trigger the Ctrl+Alt+C
		// hotkey on the way out.
		if k.flags&LLKHF_INJECTED == 0 {
			switch wParam {
			case WM_KEYDOWN, WM_SYSKEYDOWN:
				switch k.vkCode {
				case VK_F5, VK_F6, VK_F8:
					select {
					case keyEvents <- k.vkCode:
					default:
					}
					// F8 is bound to a clipboard action — swallow it so the
					// focused app (which the synthesized Ctrl+C will target)
					// does not also see F8 and clobber the selection.
					if k.vkCode == VK_F8 {
						return 1
					}
				case VK_S, VK_C:
					// Ctrl+Alt+S / Ctrl+Alt+C are LLM hotkeys. Plain S / C
					// (and Ctrl+C / Ctrl+S) pass through normally.
					if isKeyDown(VK_CONTROL) && isKeyDown(VK_MENU) {
						select {
						case keyEvents <- k.vkCode:
						default:
						}
						return 1
					}
				}
			case WM_KEYUP, WM_SYSKEYUP:
				if k.vkCode == VK_F8 {
					return 1
				}
			}
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return ret
}

// isKeyDown reports whether the high bit of GetAsyncKeyState is set,
// i.e., whether the key is currently physically (or virtually) held.
func isKeyDown(vk uintptr) bool {
	r, _, _ := procGetAsyncKeyState.Call(vk)
	return r&0x8000 != 0
}

var hookCallback = syscall.NewCallback(keyboardHookProc)

func keyboardHookProcF5(nCode uintptr, wParam uintptr, lParam uintptr) uintptr {
	if int32(nCode) == 0 && (wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN) {
		k := *(**kbdllhookstruct)(unsafe.Pointer(&lParam))
		if k.vkCode == VK_F5 {
			select {
			case keyEvents <- k.vkCode:
			default:
			}
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return ret
}

var hookCallbackF5 = syscall.NewCallback(keyboardHookProcF5)

// runKeyboardHook installs a low-level keyboard hook and pumps messages
// so Windows can dispatch hook callbacks to this thread. F5 and F6 are
// observed only; F8 is swallowed so the focused app doesn't act on it
// while we're synthesizing Ctrl+C/Ctrl+V against the same window.
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

// ensureSingleInstance opens a named mutex. If another mrgraise process
// owns it, returns false. The mutex handle leaks intentionally — it's
// released when the process exits, which is exactly when we want to
// release it.
func ensureSingleInstance() bool {
	name, _ := syscall.UTF16PtrFromString("Local\\mrgraise-singleinstance")
	handle, _, err := procCreateMutexW.Call(
		0, // lpMutexAttributes — default security
		0, // bInitialOwner — don't acquire ownership
		uintptr(unsafe.Pointer(name)),
	)
	if handle == 0 {
		// CreateMutex itself failed — don't block startup over it.
		return true
	}
	if errno, ok := err.(syscall.Errno); ok && errno == ERROR_ALREADY_EXISTS {
		return false
	}
	return true
}

func main() {
	if !ensureSingleInstance() {
		// Blocking MessageBox so the user sees the reason even when the
		// program is launched by double-click and the console flashes shut.
		title, _ := syscall.UTF16PtrFromString("mrgraise")
		body, _ := syscall.UTF16PtrFromString("mrgraise is already running. Please close the existing instance first.")
		procMessageBoxW.Call(
			0,
			uintptr(unsafe.Pointer(body)),
			uintptr(unsafe.Pointer(title)),
			MB_OK|MB_ICONERROR|MB_TOPMOST|MB_SETFOREGROUND,
		)
		return
	}

	flag.BoolVar(&debug, "debug", false, "enable verbose debug output for the F5 cycle handler")
	flag.BoolVar(&debug, "d", false, "shorthand for --debug")
	flag.Parse()

	fmt.Printf("Pinning to top: %s\n", WIN_TITLE)
	fmt.Println()
	fmt.Println(`Hotkey: F5 cycles through Report Viewer, Order Viewer, and Patient Record/Worklist. 
In order for this shortcut to work, enable 'Auto Open Order Viewer', 'Auto Open Report Viewer', and 'Auto Open ER Panel' in Preferences->Start-up.`)
	fmt.Println()
	fmt.Println("Hotkey: F6 copies patient info (Name;DOB;Loc;MRN;Date;ACC;Exam) from Order Viewer to the clipboard.")
	fmt.Println()
	fmt.Println("Hotkey: F8 takes the currently selected text, strips any prior numbering, and pastes it back with paragraphs renumbered and properly formatted.")
	fmt.Println()
	fmt.Println("Hotkey: Ctrl+Alt+S sends the currently selected report text to Claude to generate an impression and places it on the clipboard (Ctrl+V to paste).")
	fmt.Println()
	fmt.Println("Hotkey: Ctrl+Alt+C sends the currently selected report text to Claude to check for errors and prints the result here.")
	fmt.Println()
	fmt.Println("It's ok to minimize this window to the task bar, or keep it in the background, but do not close it.")
	fmt.Println()
	fmt.Println("To quit, press Ctrl-C, or click the [X] in the top right corner.")
	fmt.Println()

	go runKeyboardHook()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Each step returns true if it found and raised a window. A step that
	// returns false is skipped so F5 advances to the next available
	// window instead of being a no-op.
	cycleSteps := []func() bool{
		func() bool {
			hwnd := findWindowByPrefix("Report Viewer:")
			if hwnd == 0 {
				debugf("    [Report Viewer:] not found\n")
				return false
			}
			debugf("    [Report Viewer:] hwnd=%#x title=%q\n", hwnd, getWindowText(hwnd))
			raiseWindow(hwnd)
			return true
		},
		func() bool {
			merges := findAllWindowsByPrefix("Merge:  Radiologist") // yes, two spaces!
			debugf("    [Merge] %d match(es)\n", len(merges))
			if debug {
				for _, h := range merges {
					debugf("      %#x %q\n", h, getWindowText(h))
				}
			}
			for _, hwnd := range merges {
				raiseWindow(hwnd)
			}

			// raise the main worklist window LAST
			// so it's on top of the others.
			raiseWindow(findWindowByPrefix("Merge RealTime"))

			if len(merges) == 0 {
				debugf("    [Merge] not found\n")
				// return false
			}
			return true
		},
		func() bool {
			hwnd := findWindowByPrefix("Order Viewer:")
			if hwnd == 0 {
				debugf("    [Order Viewer:] not found\n")
				return false
			}
			debugf("    [Order Viewer:] hwnd=%#x title=%q\n", hwnd, getWindowText(hwnd))
			raiseWindow(hwnd)
			return true
		},
	}

	const cycleIdleReset = 10 * time.Second
	cycle := 0
	var lastF5 time.Time
	for {
		select {
		case <-ticker.C:
			pinTop(findWindowExact(WIN_TITLE))
		case vk := <-keyEvents:
			switch vk {
			case VK_F5:
				debugf("F5: cycle=%d, idle=%v\n", cycle, time.Since(lastF5))
				if debug {
					dumpAllTitles()
				}
				if time.Since(lastF5) >= cycleIdleReset {
					cycle = 0
				}
				for tried := 0; tried < len(cycleSteps); tried++ {
					idx := (cycle + tried) % len(cycleSteps)
					debugf("  step %d:\n", idx)
					if cycleSteps[idx]() {
						cycle = (idx + 1) % len(cycleSteps)
						break
					}
				}
				lastF5 = time.Now()
			case VK_F6:
				copyOrderInfoToClipboard()
			case VK_F8:
				renumberSelectionViaClipboard()
			case VK_S:
				runLLMOnSelectionAsync(true)
			case VK_C:
				runLLMOnSelectionAsync(false)
			}
		}
	}
}

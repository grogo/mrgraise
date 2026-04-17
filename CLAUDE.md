# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Purpose

Single-file Go utility for Windows that keeps a specific window (title hardcoded as `ER WorkFlow Panel`) pinned always-on-top. Every 3 seconds it finds the window via the Win32 API, restores it if minimized, and re-applies `HWND_TOPMOST` — without stealing focus (`SWP_NOACTIVATE`). No external dependencies beyond the Go standard library and `user32.dll`.

## Commands

- `make` (default target `build`) — runs `fmt` → `vet` → cross-compiles `mrgraise.exe` for `windows/amd64`.
- `make fmt` / `make vet` — individual steps.
- `go run main.go` — only runs on Windows; `user32.dll` calls are not portable.

There are no tests.

## Architecture notes

- The watched title is a compile-time constant (`WIN_TITLE` in `main.go`). Changing the target window means rebuilding.
- `procSetWindowPos` is called on every tick even when the window is already topmost — this is intentional, since other apps can demote it. Don't add a "skip if already topmost" guard without understanding why the polling approach was chosen over a one-shot call.
- The `SWP_NOACTIVATE` flag is load-bearing: an earlier implementation stole focus, which is why the commit history references "without stealing focus". Preserve this behavior.

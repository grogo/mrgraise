.DEFAULT_GOAL := build

.PHONY:fmt vet build manifest
fmt:
	go fmt ./...

vet: fmt
	go vet ./...

# Bakes mrgraise.exe.manifest into rsrc.syso so the Common-Controls v6
# dependency is linked into the binary; without this walk fails with
# "TTM_ADDTOOL failed" when the .manifest is not alongside the .exe.
# Requires the rsrc tool: go install github.com/akavel/rsrc@latest
manifest: rsrc.syso
rsrc.syso: mrgraise.exe.manifest
	rsrc -manifest mrgraise.exe.manifest -o rsrc.syso

build: vet manifest
	go build -o mrgraise.exe

# cross-compile for windows (fails witout Win32 libs)
build-win: vet manifest
	GOOS=windows GOARCH=amd64 go build -o mrgraise.exe

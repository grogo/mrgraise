.DEFAULT_GOAL := build

.PHONY:fmt vet build
fmt:
	go fmt ./...

vet: fmt
	go vet ./...

build: vet
	GOOS=windows GOARCH=amd64 go build -o mrgraise.exe main.go

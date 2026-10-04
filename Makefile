GOARCH_HOST := $(shell go env GOARCH)
LDFLAGS := -s -w -X main.version=$(shell git describe --always --dirty 2>/dev/null || echo dev)

.PHONY: build linux test lint hooks e2e-sim lab-e2e clean

# Native build (macOS uses the sim driver).
build:
	go build -ldflags '$(LDFLAGS)' -o bin/ ./cmd/burstyd ./cmd/bursty

# Linux binaries for the host arch (or ARCH=amd64|arm64), plus the static guest init.
ARCH ?= $(GOARCH_HOST)
linux:
	mkdir -p bin/linux-$(ARCH)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -ldflags '$(LDFLAGS)' -o bin/linux-$(ARCH)/ ./cmd/burstyd ./cmd/bursty ./cmd/bursty-init

test:
	go vet ./...
	go test -race -count=1 ./...

# gofmt check + vet for the host and for Linux (the firecracker driver and guest init are Linux-only).
lint:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run: gofmt -w ."; exit 1; }
	go vet ./...
	GOOS=linux go vet ./...

# Use the repo's git hooks (.githooks/pre-commit).
hooks:
	git config core.hooksPath .githooks

# End-to-end CLI run against a throwaway sim daemon.
e2e-sim: build
	scripts/e2e.sh sim

# Real Firecracker inside the emulated KVM lab (for Macs without nested virtualization).
lab-e2e:
	scripts/lab-e2e.sh

clean:
	rm -rf bin

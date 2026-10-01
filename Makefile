MODULE  := github.com/anaryk/proxmox-cloudflared-operator
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: build test lint fmt

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/pco ./cmd/pco

test:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

MODULE  := github.com/anaryk/proxmox-cloudflared-operator
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# Without it gin links a msgpack codec the API never uses, about 6 MB of binary.
# Keep it in sync with run.build-tags in .golangci.yml, builds.tags in
# .goreleaser.yaml and the test job in CI.
TAGS := nomsgpack

.PHONY: build test lint fmt test-scripts snapshot package e2e-binaries

build:
	go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o bin/pco ./cmd/pco

test:
	go test -tags $(TAGS) -race ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

test-scripts:
	bash scripts/install_test.sh
	bash packaging/release-key_test.sh
	bash packaging/is-latest_test.sh
	shellcheck scripts/*.sh packaging/*.sh packaging/scripts/*.sh

# Builds the .deb files without a tag and without publishing or signing anything.
# Keep the version in sync with the release and ci workflows.
snapshot:
	go run github.com/goreleaser/goreleaser/v2@v2.18.2 release --snapshot --clean --skip=sign
	packaging/check-artifacts.sh

package: snapshot
	@echo "the .deb files are in dist/:"
	@ls dist/*.deb

# The end-to-end suite and a binary for a Proxmox VE node; test/e2e/README.md
# says how to run them.
e2e-binaries:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o bin/pco-linux-amd64 ./cmd/pco
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags e2e,$(TAGS) -o bin/e2e.test ./test/e2e

MODULE  := github.com/anaryk/proxmox-cloudflared-operator
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

# Without nomsgpack gin links a msgpack codec the API never uses, about 6 MB of
# binary. Keep it in sync with run.build-tags in .golangci.yml, builds.tags in
# .goreleaser.yaml and the test job in CI. webui embeds the interface make ui
# builds into internal/web/ui/dist. Only UI=1 asks for it, never what is on
# disk, so a build that should carry the interface fails instead of shipping
# the page that says it has none.
comma := ,
UI ?= 0
TAGS := nomsgpack$(if $(filter 1,$(UI)),$(comma)webui)

.PHONY: build test lint fmt test-scripts snapshot package e2e-binaries scale ui ui-dist ui-test ui-budget

build: $(if $(filter 1,$(UI)),ui-dist)
	go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o bin/pco ./cmd/pco

# The Go tests never need the interface, whatever UI says.
test:
	go test -tags nomsgpack -race ./...

# Measures cycles of the engine at 100 to 2000 routes and prints the tables, see
# test/scale/README.md. It takes several minutes; SCALE_DIR keeps its store on
# another disk.
scale:
	go test -tags nomsgpack,scale -run 'TestScale|TestDefaultLimiter|TestDiskCost' -count=1 -v -timeout 10m ./test/scale/

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

test-scripts:
	bash scripts/install_test.sh
	bash packaging/release-key_test.sh
	bash packaging/is-latest_test.sh
	bash packaging/check-artifacts_test.sh
	bash packaging/release-workflow_test.sh
	shellcheck scripts/*.sh packaging/*.sh packaging/scripts/*.sh

# The frontend in web/, with Node and npm at the versions of web/.nvmrc and
# web/package.json. The packages are installed once, and again when the lock
# file changes; a failed install leaves nothing behind, so the next make tries
# again. ui-test needs no build, ui-budget measures the one ui makes.
web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --ignore-scripts || { rm -rf node_modules; exit 1; }
	@touch web/node_modules

ui: web/node_modules
	cd web && npm run build

ui-dist:
	@test -f internal/web/ui/dist/index.html || { echo "internal/web/ui/dist is missing: run make ui" >&2; exit 1; }

ui-test: web/node_modules
	cd web && npm run lint && npm run typecheck && npm test && npm run licences

ui-budget: ui
	cd web && npm run budget

# Builds the interface and the .deb files without a tag and without publishing
# or signing anything. Reading the binaries back takes dpkg-deb and go.
# Keep the version in sync with the release and ci workflows.
snapshot: ui
	go run github.com/goreleaser/goreleaser/v2@v2.18.2 release --snapshot --clean --skip=sign
	packaging/check-artifacts.sh --require-ui

package: snapshot
	@echo "the .deb files are in dist/:"
	@ls dist/*.deb

# The end-to-end suite and a binary for a Proxmox VE node; test/e2e/README.md
# says how to run them.
e2e-binaries: $(if $(filter 1,$(UI)),ui-dist)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o bin/pco-linux-amd64 ./cmd/pco
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags e2e,$(TAGS) -o bin/e2e.test ./test/e2e

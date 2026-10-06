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

.PHONY: build test lint fmt test-scripts snapshot package template e2e-binaries scale ui ui-dist ui-test ui-budget ui-e2e ui-words ui-types

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

# check-refs.sh fails on a citation of notes that are not in the repository.
lint:
	golangci-lint run
	bash scripts/check-refs.sh

fmt:
	golangci-lint fmt

test-scripts:
	bash scripts/install_test.sh
	bash scripts/check-refs_test.sh
	bash packaging/release-key_test.sh
	bash packaging/release-key-file_test.sh
	bash packaging/is-latest_test.sh
	bash packaging/check-artifacts_test.sh
	bash packaging/release-workflow_test.sh
	bash packaging/cloudflared-versions_test.sh
	bash packaging/appliance/pin_test.sh
	bash packaging/appliance/overlay_test.sh
	shellcheck scripts/*.sh packaging/*.sh packaging/scripts/*.sh packaging/appliance/*.sh

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

# The browser suite (web/e2e): the real pco web with the interface of
# internal/web/ui/dist, the fake daemon and the fake Proxmox VE of hack/
# behind it, in the browsers Playwright installs (node_modules/.bin/playwright
# install, once). E2E passes its flags on, such as E2E='--project=chromium-*'.
ui-e2e: web/node_modules ui-dist
	go build -tags nomsgpack,webui -trimpath -ldflags "$(LDFLAGS)" -o bin/pco ./cmd/pco
	go build -tags nomsgpack -trimpath -o bin/fakepco ./hack/fakepco
	go build -tags nomsgpack -trimpath -o bin/fakepve ./hack/fakepve
	cd web && node_modules/.bin/playwright test $(E2E)

# The word tables of internal/present for the interface, generated from the Go
# code and committed. A failed run leaves the committed file as it was.
ui-words:
	go run ./hack/tsgen -words > web/src/gen/words.gen.ts.tmp && mv web/src/gen/words.gen.ts.tmp web/src/gen/words.gen.ts || { rm -f web/src/gen/words.gen.ts.tmp; exit 1; }

# The types of the JSON of the daemon and of the web process, the same way.
ui-types:
	go run ./hack/tsgen -types > web/src/api/types.gen.ts.tmp && mv web/src/api/types.gen.ts.tmp web/src/api/types.gen.ts || { rm -f web/src/api/types.gen.ts.tmp; exit 1; }

# Builds the interface and the .deb files without a tag and without publishing
# or signing anything, and the appliance template where mmdebstrap is
# installed. Reading the binaries back takes dpkg-deb and go.
# Keep the version in sync with the release and ci workflows.
snapshot: ui
	go run github.com/goreleaser/goreleaser/v2@v2.18.2 release --snapshot --clean --skip=sign
	packaging/check-artifacts.sh --require-ui
	@if command -v mmdebstrap >/dev/null 2>&1; then \
		$(MAKE) template; \
	else \
		echo "mmdebstrap is not installed, so there is no appliance template; packaging/appliance/README.md says how to build one"; \
	fi

package: snapshot
	@echo "the .deb files are in dist/:"
	@ls dist/*.deb

# The appliance template of the package in dist/, into build/appliance. It
# needs Linux and mmdebstrap, see packaging/appliance/README.md;
# TEMPLATE_ARCH=arm64 builds the other one.
TEMPLATE_ARCH ?= amd64
template:
	@test -f dist/metadata.json || { echo "dist/ holds no packages: run make snapshot first" >&2; exit 1; }
	version=$$(jq -r .version dist/metadata.json) && \
		packaging/appliance/build.sh --arch $(TEMPLATE_ARCH) --version "$$version" \
			--deb "dist/pco_$${version}_$(TEMPLATE_ARCH).deb" --out build/appliance

# The end-to-end suite and a binary for a Proxmox VE node; test/e2e/README.md
# says how to run them.
e2e-binaries: $(if $(filter 1,$(UI)),ui-dist)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags $(TAGS) -trimpath -ldflags "$(LDFLAGS)" -o bin/pco-linux-amd64 ./cmd/pco
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags e2e,$(TAGS) -o bin/e2e.test ./test/e2e

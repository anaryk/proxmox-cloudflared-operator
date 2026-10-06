# Development

How to build pco, run its tests, work on the web interface without a node, and send a change.
pco is one Go binary, `cmd/pco`, with the code in `internal/`; the web interface is a React
application in `web/` that the binary embeds; the documentation is `docs/`, which the site in
`site/` builds.

## What you need

- Go, at the version `go.mod` names; the toolchain line there fetches the exact one.
- `golangci-lint` 2.14, which `make lint` runs, and `shellcheck` for `make test-scripts`.
- Node 22.23.3 and npm 10.9.9, exactly, for the web interface and the site. `web/.npmrc` sets
  `engine-strict`, so `npm ci` refuses any other version. With nvm, `nvm install` in `web/` reads
  `web/.nvmrc`.
- Docker, only for `make docs-screenshots`.
- Linux, root and nftables, only for the tests that need them; elsewhere they skip.

Every build passes the Go build tag `nomsgpack`; the Makefile and CI do it for you. Without it
gin links a msgpack codec the API never uses.

## Build

| Command | Does |
|---|---|
| `make build` | `bin/pco`, without the web interface: its pages say that this build has none. |
| `make ui` | Builds the web interface with Vite into `internal/web/ui/dist`. |
| `make build UI=1` | `bin/pco` with the web interface, the build tag `webui`. It fails when `make ui` has not run, rather than build the page that says there is none. |
| `make snapshot` | The packages of every architecture into `dist/`, without a tag, a signature or a release, checked with `packaging/check-artifacts.sh --require-ui`; and the appliance template where `mmdebstrap` is installed. |
| `make e2e-binaries` | A `pco` for a Proxmox VE node, amd64, and the end-to-end suite as a binary. |

## Tests and checks

A change is ready when these pass:

    make lint test build
    GOOS=linux go vet -tags nomsgpack ./...
    make test-scripts

`make lint` is `golangci-lint` and `scripts/check-refs.sh`, which fails on a citation of material
that is not in the repository: a page, a comment or a test name says the reason in its own words.
`make test` runs every Go test with the race detector. `GOOS=linux go vet` checks the code that
builds on Linux only. `make test-scripts` runs the tests of the shell scripts, the installer and
the release, `shellcheck` and the tests of the site's scripts.

When you touch the web interface, the site or a page:

| Command | Does |
|---|---|
| `make ui-test` | `eslint`, `tsc`, the unit tests of the page with Vitest, and the licences of its packages. |
| `make ui-budget` | Builds the page and holds it to its size budget. |
| `make ui-e2e` | The browser suite: the real `pco web` with the web interface, the fakes behind it, in the browsers Playwright installs; see below. |
| `make ui-types ui-words` | Writes the TypeScript types of the daemon's JSON, `web/src/api/types.gen.ts`, and the word tables of `internal/present`, `web/src/gen/words.gen.ts`, from the Go code. Run them after a change of either, and commit what they write: CI fails when they differ. |
| `make docs docs-lint docs-test` | Builds the site, which fails on a link to a page that does not exist; holds the pages to the rules of markdownlint, refuses syntax only the site reads, checks every link down to its fragment and the images against their budget; and runs the tests of the site. |
| `make docs-cli` | Writes the command reference in `docs/cli` from the help of the commands. A test of `cmd/pco` fails when the pages differ from the help. |

The drift tests of `internal/docscheck`, part of `make test`, keep the reference pages in step
with the code: every setting has a row in [Settings](settings.md), every environment variable
the product reads a row in [Operations](operations.md#environment-variables), and every event
kind, doctor check, named problem line, route state, held reason, kind of what waits and writer
verdict an entry in [Problems](problems.md).

Some tests need what a laptop does not have, and skip without it: root, nftables, a network
namespace, `gpg`, `dpkg-deb`, `systemd-analyze`. CI runs them, and an environment variable makes a
skip a failure there: `PCO_REQUIRE_LINUX_TESTS`, `PCO_REQUIRE_CRYPTO_TESTS`, `PCO_REQUIRE_DEB_TESTS`
and `PCO_REQUIRE_SYSTEMD_TESTS`, each `1`.

How the Go tests are written: the standard `testing` package with `testify/require`, table-driven
where there are several cases, next to the code. No test reaches the network, sleeps on the wall
clock or depends on the order of a map: time is passed in, and the outside world is a fake.

## The fakes

| Fake | What it stands for |
|---|---|
| `hack/fakepco` | The daemon: the API on a unix socket, served from a scenario of `internal/api/apifake`. It runs cycles and samples traffic as the daemon does, and answers every call with the daemon's own code for digests, batching, replays and checks. |
| `hack/fakepve` | Proxmox VE as pco web asks it: the privileges, the guests a user may see, tickets, API tokens, and in the appliance passwords and second factors, over TLS. |
| `hack/cffake` | The Cloudflare API over HTTP, from the in-memory Cloudflare of the tests. |

`go run ./hack/fakepco -list` names the scenarios: `populated`, `empty`, `first-run`, `untagged`,
`tagged-empty`, `reader`, `rogue`, `frozen`, `hold`, `outage`, `egress-off`, `large` and `wide`,
or give a directory of your own. `-now` starts its clock at another time, and `-still` keeps it
there. On `-control`, loopback only, it takes calls that change what it serves; the documentation
of `apifake.Engine.Control` lists them:

    curl -X POST 127.0.0.1:7071/streams/pause

`hack/fakepve` reads its users from a file:

```json
{
  "users": [
    {
      "user": "alice@pve",
      "privileges": { "/": ["Sys.Audit", "Sys.Modify"] },
      "guests": ["qemu/101", "lxc/200"],
      "tickets": ["PVE:alice@pve:66F1E2D3::YWxpY2U="],
      "tokens": [{ "id": "pco", "secret": "0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d", "privsep": false }]
    }
  ]
}
```

A user with `password` signs in as in the appliance, and `totp` (a code that never changes),
`recovery` (codes good once) or `webauthn: true` give it a second factor.

## The web interface without a node

The quickest way to work on a page is the development server of Vite, which answers `/api` from
the fixtures of `web/src/fixtures` and changes nothing:

    cd web
    VITE_MOCK=1 npm run dev

`VITE_MOCK_STATE` names another state: `populated`, `empty`, `first-run`, `untagged`,
`tagged-empty`, `rogue`, `frozen`, `rogue-scenario`, `scenario-populated` or `large`. A write is
refused as a daemon that is busy would refuse it. The fixtures are made from the goldens of the Go
code with `npm run fixtures`, whose test fails when a golden changed and the fixtures did not.

For the real `pco web`, with its sign-in and its gateway, run it between the two fakes, as the
browser suite does. With the users above in `users.json`:

    make ui && make build UI=1
    mkdir -p /tmp/pcofake
    go run ./hack/fakepve -users users.json -cert-dir /tmp/pcofake -web-cert -node node1 &
    go run ./hack/fakepco -socket /tmp/pcofake/pco/pco.sock -scenario populated &
    bin/pco web --socket /tmp/pcofake/pco/pco.sock --listen 127.0.0.1:8643 \
      --cert /tmp/pcofake/tls.crt --key /tmp/pcofake/tls.key \
      --pin /tmp/pcofake/pveproxy.crt --pve-url https://127.0.0.1:8006/api2/json

Open `https://127.0.0.1:8643/`, accept its certificate, and sign in with the token
`alice@pve!pco=0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d`. A user with `Sys.Audit` alone and a few
guests shows what a reader sees.

## The browser suite

`make ui-e2e` builds `bin/pco` with the interface, `bin/fakepco` and `bin/fakepve`, and runs the
Playwright suite of `web/e2e` against them: for each test a fake Proxmox VE, a fake daemon with a
scenario, and the real `pco web` between them and the browser. Every test fails on a violation of
the Content-Security-Policy, on an error in the console of the page, and on an answer that carries
a header of CORS. The browsers are installed once, by the Playwright of `web/package.json`:

    cd web && node_modules/.bin/playwright install chromium firefox webkit

`E2E` passes flags on, such as `make ui-e2e E2E='--project=chromium-desktop-light'`. The projects
are Chromium, Firefox and WebKit at the size of a desktop and of a phone, Chromium in both colour
schemes, and `perf`, which holds the flow map to its budgets. Start Playwright from
`node_modules/.bin/playwright`, never with `npx`, which may fetch a package that is not pinned.

`make docs-screenshots` takes the screenshots of these pages from the same harness, in the
Playwright container of the pinned version, for amd64 on every machine, so the pixels come from
that image alone. It needs Docker, and on another architecture an emulator of amd64 that Docker
runs images with. It writes a file only when its picture changed.

## Sending a change

- Work on a branch of your own and open a pull request against `main`. CI runs on every push and
  pull request: lint, the tests, the Linux tests as root, the scripts, the build with and without
  the web interface, the web interface and its browser suite, the site, the packages and the
  appliance template.
- One change a commit, with what it needs: the code, its tests, and the pages it changes. A
  change of behaviour that a page describes changes the page in the same pull request.
- A commit subject is `area: what it does`, in lower case, in the imperative, at most 72
  characters, without a full stop: `ui: keep the focus clear of an opened events strip`. The area
  is the package or the part, such as `engine`, `api`, `ui`, `docs`, `cli`, `test`. A body says
  why, when the subject cannot.
- English, plain, for a Proxmox admin. A comment says why, not what. Example data is that of the
  pages: names under `example.com`, addresses of the documentation ranges and of `10.0.0.0/8`,
  VMIDs from 100 to 199, ids that are visibly made up.
- A new dependency needs a reason. The page's runtime dependencies may carry the licences MIT,
  ISC, BSD-2-Clause, BSD-3-Clause, Apache-2.0, 0BSD or CC0 only, which `make ui-test` checks.

Raising Node and npm is in
[RELEASING](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/packaging/RELEASING.md#node-and-npm).

## More

- [The end-to-end suite](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/test/e2e/README.md):
  pco as installed on a Proxmox VE node, against a fake Cloudflare or a zone of yours.
- [The scale benchmark](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/test/scale/README.md):
  the cycle of the daemon at 100 to 2000 routes, `make scale`.
- [The performance of the flow map](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/web/e2e/perf/README.md):
  the map against a baseline, and its budgets.
- [The appliance template](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/packaging/appliance/README.md):
  what it holds and how to build it.
- [Releasing](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/packaging/RELEASING.md):
  the release workflow, its key, the web interface in a release, and the site.
- [The documentation site](https://github.com/anaryk/proxmox-cloudflared-operator/blob/main/site/README.md):
  its commands, what a page may use, and its navigation.
- [Architecture](architecture.md) and [API](api.md), for the parts and the calls between them.

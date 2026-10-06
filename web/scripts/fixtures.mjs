// Makes the fixtures of the page, src/fixtures/*.json, from the goldens of
// the Go code: the states, events and notices the engine's tests pin, the
// answers of the API and of pco web. A fixture is a golden, or a golden with
// the few fields changed that make its case, so it has every field the
// daemon writes. Run it with npm run fixtures after a golden changed; the
// test of this script fails until then.
//
// largeState() makes a state of many routes for the mock server, which is
// too big to keep in the repository.

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const root = new URL('../../', import.meta.url)
export const fixturesDir = fileURLToPath(new URL('../src/fixtures/', import.meta.url))

const read = (path) => JSON.parse(readFileSync(new URL(path, root), 'utf8'))

const clone = (v) => structuredClone(v)

// A state after a cycle in which no guest names a hostname.
function noRoutes(populated) {
  return {
    ...clone(populated),
    digest: '3b9d0c6e1f2a4d57',
    complete: true,
    routes: [],
    issues: [],
    actions: [],
    conflicts: [],
    lost: [],
    problems: [],
    waiting: [],
    unapproved: [],
    segments: [],
    rogueConnectors: [],
    hold: undefined,
    offer: undefined,
    admission: 'tag',
    gateTagged: 0,
    egress: { state: 'on', since: populated.egress.since },
  }
}

// A state in which nothing needs a person but two connectors pco does not
// run, which Cloudflare lists on the tunnel of the install.
function rogue(populated) {
  const st = clone(populated)
  const [first] = st.rogueConnectors
  return {
    ...st,
    digest: '8c41f0a7d2e95b36',
    routes: st.routes.filter((r) => r.state === 'active'),
    unapproved: [],
    credentials: st.credentials.map((c) => ({ ...c, checked: true, report: clone(st.credentials[0].report) })),
    zones: st.zones.filter((z) => z.state !== 'frozen'),
    rogueConnectors: [first, { ...first, id: '7a2c4e91-5d3b-4f80-9e16-2b8d0c5a7f43', originIp: '203.0.113.9', since: '2026-10-01T11:58:00Z' }],
  }
}

// doctorFindings is what GET /v1/doctor answers an admin for the populated
// state, in the words internal/doctor writes: a failure with a command to
// run, warnings whose fix is a command, advice in words and a placeholder,
// and checks that passed.
function doctorFindings() {
  return [
    { check: 'mode', level: 'ok', detail: 'enforce: changes are applied' },
    { check: 'cycle', level: 'ok', detail: 'the last cycle ran 8s ago' },
    { check: 'problems', level: 'fail', detail: '1 problem: a problem', fix: 'pco status' },
    {
      check: 'egress',
      level: 'fail',
      detail: 'the egress filter is switched off since 2026-10-01T11:00:00Z: the connectors are not confined',
      fix: 'pco egress on',
    },
    { check: 'connector pco-abc123', level: 'fail', detail: 'not connected to Cloudflare', fix: 'journalctl -u pco-cloudflared@pco-abc123' },
    { check: 'credential cred1', level: 'warn', detail: 'not checked yet', fix: 'pco credential check cred1' },
    { check: 'approval lxc/201', level: 'warn', detail: 'new-1 (lxc/201) waits for approval', fix: 'pco guest approve lxc/201' },
    { check: 'approval lxc/202', level: 'warn', detail: 'dns-1 (lxc/202) waits for approval', fix: 'pco guest approve lxc/202' },
    {
      check: 'waiting',
      level: 'warn',
      detail: '4 things wait for a confirmation: mass delete guard: 7 of 9 records are being removed',
      fix: 'pco apply --confirm-deletes',
    },
    { check: 'conflicts', level: 'warn', detail: '1 record of someone else stands in the way: api.example.com', fix: 'pco adopt <name>' },
    { check: 'writer', level: 'ok', detail: 'this daemon writes the tunnel configuration' },
    { check: 'cloudflared', level: 'ok', detail: 'cloudflared 2026.9.3' },
    { check: 'proxmox', level: 'ok', detail: 'Proxmox VE 9.0' },
  ]
}

// traffic is the answer of GET /v1/traffic for a state: 15 minutes of samples
// for each tunnel that has a connector, and a figure for each active route.
export function trafficOf(state, at = state.at) {
  const end = Date.parse(at)
  const tunnels = state.connectors.map((c, n) => ({
    tunnelId: c.tunnelId,
    node: state.node,
    cloudflared: '2026.9.3',
    configVersion: 3,
    haConnections: c.connections,
    edges: [
      { connection: 0, location: 'fra08' },
      { connection: 1, location: 'prg01' },
    ],
    rttMs: [11.2, 18.9],
    stale: false,
    samples: Array.from({ length: 180 }, (_, i) => ({
      at: new Date(end - (179 - i) * 5000).toISOString().replace('.000Z', 'Z'),
      rps: Math.round((30 + 10 * Math.sin((i + n * 7) / 9)) * 10) / 10,
      errorsPerSec: i % 40 === 0 ? 0.4 : 0.1,
      concurrent: 3,
    })),
  }))
  const routes = state.routes
    .filter((r) => r.state === 'active' && r.service)
    .map((r) => ({
      hostname: r.hostname,
      owner: r.owner,
      target: r.service.replace(/^[a-z]+:\/\//, ''),
      flowsPerSec: 2.4,
      stale: false,
    }))
  return { at, interval: '5s', tunnels, routes, routesTotal: routes.length }
}

// largeState is the populated state with n routes, most of them active, as
// an install of that size would have them.
export function largeState(n = 1000) {
  const populated = read('internal/engine/testdata/state_populated.json')
  const [template] = populated.routes
  const states = ['active', 'active', 'active', 'active', 'active', 'active', 'unreachable', 'withdrawn', 'no-zone', 'held']
  const routes = Array.from({ length: n }, (_, i) => {
    const vmid = 1000 + Math.floor(i / 2)
    const state = states[i % states.length]
    const host = `app-${String(i).padStart(4, '0')}.example.com`
    const addr = `10.${Math.floor(vmid / 250) % 256}.${vmid % 250}.${(i % 2) + 10}`
    return {
      ...clone(template),
      hostname: host,
      owner: `qemu/${vmid}`,
      state,
      reason: state === 'active' ? undefined : `${state}: as the large fixture has it`,
      service: `http://${addr}:8080`,
      warnings: undefined,
      guest: { kind: 'qemu', vmid, name: `app-${vmid}` },
      candidates: [{ addr, source: 'static', ok: true, level: 'port' }],
      rule: { hostname: host, service: `http://${addr}:8080` },
      path: { ...template.path, port: `tap${vmid}i0` },
    }
  })
  return { ...populated, digest: 'f00dfacecafe0001', routes }
}

export function fixtures() {
  const populated = read('internal/engine/testdata/state_populated.json')
  const empty = read('internal/engine/testdata/state_empty.json')
  const events = read('internal/engine/testdata/events.json')
  const version = read('internal/api/testdata/version.json')
  const untagged = noRoutes(populated)
  const last = events[events.length - 1]
  return {
    // engine.State
    populated,
    empty,
    untagged,
    'tagged-empty': { ...clone(untagged), digest: '6d0e2b4a9c7f1835', admission: 'approve', gateTagged: 3 },
    rogue: rogue(populated),
    // engine.Event, engine.Hello, engine.TrafficView
    events,
    hello: { boot: last.boot, version: version.version, seq: last.seq, digest: populated.digest, pollInterval: version.pollInterval },
    traffic: trafficOf(populated, '2026-10-01T12:00:05Z'),
    // the answers of the API
    version,
    settings: read('internal/api/testdata/settings.json'),
    guests: read('internal/api/testdata/guests.json'),
    annotation: read('internal/api/testdata/annotation.json'),
    'manual-routes': read('internal/api/testdata/manual_routes.json'),
    claims: read('internal/engine/testdata/claims.json'),
    approvals: read('internal/engine/testdata/approvals.json'),
    'apply-result': read('internal/engine/testdata/apply_result.json'),
    // doctor.Finding for an admin, wire.DoctorCounts for a reader
    doctor: doctorFindings(),
    'doctor-counts': read('internal/web/wire/testdata/doctor_counts.json'),
    // pco web's own
    session: read('internal/web/wire/testdata/session.json'),
    unauthenticated: read('internal/web/wire/testdata/unauthenticated.json'),
    upstream: read('internal/web/wire/testdata/upstream.json'),
  }
}

// The text of a fixture as the file holds it.
export const text = (value) => `${JSON.stringify(value, null, 2)}\n`

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  mkdirSync(fixturesDir, { recursive: true })
  for (const [name, value] of Object.entries(fixtures())) {
    writeFileSync(`${fixturesDir}${name}.json`, text(value))
  }
}

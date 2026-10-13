// The install the screenshots of the documentation show: the populated
// scenario of the fake daemon, a route in every state, with the names and ids
// the pages of docs/ use, and what the test data has as a placeholder written
// as the daemon writes it. The fake daemon's clock stands at now, the
// browser's a few seconds later.

import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

import type { RouteView, State } from '../../src/api/types.gen'

// now is the moment of the scenario, when its last cycle ended; the page is
// looked at seconds later.
export const now = '2026-10-01T12:00:02Z'
export const pageTime = '2026-10-01T12:00:05Z'

// node is the name of the node, as the pages of docs/ name the nodes of their
// examples: the fake Proxmox VE and pco web are given it too.
export const node = 'node1'

const populated = fileURLToPath(new URL('../../../internal/api/apifake/testdata/scenarios/populated/', import.meta.url))

// The names of the test data, and those the pages use: the node, the
// accounts, zones, credentials and tunnel as Cloudflare and pco name them,
// the zone of a registrable name as one of the names kept for examples, and a
// data centre of Cloudflare's other than the one nearest the lab. Each is a
// whole word of the data.
const names: [RegExp, string][] = [
  [/\bpve1\b/g, node],
  [/\bacc1\b/g, '0123456789abcdef0123456789abcdef'],
  [/\bacc2\b/g, '00112233445566778899aabbccddeeff'],
  [/\bacc3\b/g, 'fedcba9876543210fedcba9876543210'],
  [/\bacc4\b/g, '0f1e2d3c4b5a69788796a5b4c3d2e1f0'],
  [/\bzone1\b/g, 'abcdef0123456789abcdef0123456789'],
  [/\bzone2\b/g, 'bcdef0123456789abcdef0123456789a'],
  [/\bzone3\b/g, 'cdef0123456789abcdef0123456789ab'],
  [/\bcred1\b/g, 'a1b2c3d4'],
  [/\bcred2\b/g, 'e5f6a7b8'],
  [/\btoken-1\b/g, '9f8e7d6c5b4a39281706f5e4d3c2b1a0'],
  [/\babc123\b/g, '7f3a9c0d41b2'],
  [/\bexample\.info\b/g, 'example.net'],
  [/\bprg01\b/g, 'ams01'],
]

type Json = Record<string, unknown>

const read = <T>(file: string): T => JSON.parse(readFileSync(join(populated, file), 'utf8')) as T

// edit writes what the test data has as a placeholder or out of order as the
// daemon would have it, before the names are replaced.
function edit(files: Map<string, unknown>): void {
  const st = files.get('state.json') as State
  // A fourth zone would need a fourth name kept for examples: the zone that
  // was not served goes, and the frozen one takes its name. The guest whose
  // hostname is in no zone at all goes too; wiki.example.org shows that state.
  st.zones = st.zones.filter((z) => z.name !== 'example.net')
  st.routes = st.routes.filter((r) => r.owner !== 'qemu/130')
  for (const r of st.routes) {
    if (r.hostname === 'www.example.com') delete r.warnings
    if (r.hostname === 'www.store.example.com') r.warnings = ['more than one level below example.com: needs an advanced certificate']
    if (r.hostname === 'old.example.com') r.reason = 'no longer claimed by qemu/101 since 2026-10-01T11:59:30Z; released after the grace period'
  }
  for (const a of st.actions) if (a.held) a.held = 'grace period: 28s left'
  st.problems = [
    'tunnel pco-abc123 in account acc1 is served by connector 0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807 from 198.51.100.7 (cloudflared 2026.8.0), ' +
      'which pco does not run on this node: it takes a share of the requests to every hostname of the tunnel; ' +
      'unless you run it, rotate the tunnel secret with pco tunnel rotate --account acc1',
    'zone example.net is no longer listed by credential cred1; account acc3 is left as it is until the zone is listed again ' +
      'or pco apply --confirm-deletes confirms it is gone',
  ]
  st.issues = [{ guest: { kind: 'qemu', vmid: 103 }, line: 5, col: 19, msg: 'invalid target ":8080/builds": paths are not supported' }]
  // The checks a deep check of the credential makes, all of them passed.
  const report = st.credentials.find((c) => c.id === 'cred1')?.report
  if (report) {
    report.checks = [
      { capability: 'token', ok: true },
      { capability: 'accounts', ok: true },
      { capability: 'zones', ok: true },
      { capability: 'dns.read', scope: 'example.com', scopeId: 'zone1', ok: true },
      { capability: 'dns.write', scope: 'example.com', scopeId: 'zone1', ok: true },
      { capability: 'tunnel.read', scope: 'Main', scopeId: 'acc1', ok: true },
      { capability: 'tunnel.write', scope: 'Main', scopeId: 'acc1', ok: true },
    ]
    report.leftovers = []
  }

  const claims = files.get('claims.json') as Json[]
  for (const c of claims) {
    if (c.hostname === 'old.example.com') c.missingSince = '2026-10-01T11:59:30Z'
    for (const w of (c.waiting as Json[] | undefined) ?? []) w.since = '2026-10-01T11:59:00Z'
  }

  // web-1 has the identity it was approved in, as the others have theirs.
  const guests = (files.get('guests.json') as Json[]).filter((g) => g.ref !== 'qemu/130')
  for (const g of guests) if (g.ref === 'qemu/101') g.identity = 'uuid:101'
  files.set('guests.json', guests)
  const notes = files.get('notes.json') as Record<string, string>
  delete notes['qemu/130']
  notes['qemu/103'] = 'Build server of the team.\nAsk ops before a change.\n\n```cf-tunnel\nci.example.com -> :8080/builds\n```\n'

  for (const ev of files.get('events.json') as Json[]) {
    if (ev.subject === 'www.example.com' && ev.message === 'qemu/101: unreachable') {
      ev.level = 'info'
      ev.message = 'qemu/101: active'
    }
  }

  // The requests of the last 15 minutes, a sample every 5 s, which the test
  // data has for one: they rise and fall, with a few errors for a minute,
  // and end at the figures of the data.
  const tunnel = (files.get('traffic.json') as { tunnels: { samples: Json[] }[] }).tunnels[0]
  if (tunnel) {
    const last = tunnel.samples[tunnel.samples.length - 1] ?? {}
    const end = Date.parse(now)
    const samples: Json[] = []
    for (let i = 0; i < 180; i++) {
      const rps = Math.round(10 * (37 + 6 * Math.sin(i / 17) + 2 * Math.sin(i / 3.7))) / 10
      const errors = i >= 70 && i < 82 ? 1.5 : 0.1
      samples.push({ at: new Date(end - (179 - i) * 5000).toISOString().replace('.000', ''), rps, errorsPerSec: errors, concurrent: Math.round(rps / 12) })
    }
    samples[179] = last
    tunnel.samples = samples
  }
}

// write writes the scenario into dir, as fakepco -scenario reads one. The
// scenario of the setup has no events, and the settings of a new install.
export function write(dir: string, setup = false): void {
  const files = new Map<string, unknown>()
  for (const name of readdirSync(populated)) files.set(name, read(name))
  edit(files)
  if (setup) {
    files.set('events.json', [])
    files.set('manual_routes.json', [])
    files.set('settings.json', JSON.parse(readFileSync(join(populated, '../first-run/settings.json'), 'utf8')))
  }
  mkdirSync(dir, { recursive: true })
  for (const [name, value] of files) {
    let text = JSON.stringify(value, null, 2)
    for (const [from, to] of names) text = text.replace(from, to)
    writeFileSync(join(dir, name), text + '\n')
  }
}

// The scenarios of this worker, written once into a directory that goes
// when the worker ends.
let dir: string | undefined
function written(): string {
  if (dir) return dir
  const made = mkdtempSync(join(tmpdir(), 'pco-docs-'))
  process.once('exit', () => rmSync(made, { recursive: true, force: true }))
  write(join(made, 'docs'))
  write(join(made, 'setup'), true)
  dir = made
  return dir
}

export const scenarioDir = (): string => join(written(), 'docs')
export const setupDir = (): string => join(written(), 'setup')

// state is the state of the scenario, to change for a step of the setup.
export function state(): State {
  return JSON.parse(readFileSync(join(scenarioDir(), 'state.json'), 'utf8')) as State
}

// firstRun is the state of a node set up a moment ago, observing only: a
// cycle ran, and there is no credential yet.
export function firstRun(): State {
  const st = state()
  return {
    ...st,
    mode: 'observe',
    complete: true,
    routes: [],
    issues: [],
    tunnels: [],
    connectors: [],
    credentials: [],
    zones: [],
    actions: [],
    conflicts: [],
    lost: [],
    problems: ['no Cloudflare credential; add one with pco credential add'],
    waiting: [],
    offer: '',
    unapproved: [],
    segments: [],
    gateTagged: 0,
    rogueConnectors: [],
  }
}

const account = '0123456789abcdef0123456789abcdef'
const credential = 'a1b2c3d4'
const tunnel = 'pco-7f3a9c0d41b2'

// observing is the state of a new install once its credential is added and
// its zones listed, observing only, with no route and no tunnel yet.
export function observing(): State {
  const st = state()
  return {
    ...st,
    mode: 'observe',
    routes: [],
    issues: [],
    tunnels: [],
    connectors: [],
    credentials: st.credentials.filter((c) => c.id === credential),
    zones: st.zones
      .filter((z) => z.accountId === account)
      .map((z) => ({ ...z, credentials: [credential], pinned: undefined, stale: [], excluded: z.excluded.filter((c) => c === credential) })),
    actions: [],
    conflicts: [],
    lost: [],
    problems: [],
    waiting: [],
    offer: '',
    unapproved: [],
    segments: [],
    rogueConnectors: [],
  }
}

// firstRoute is the route of the setup's example guest, and what the cycle
// that found it plans while pco observes, as pco plan lists it.
export const firstRoute: RouteView = {
  hostname: 'docs.example.com',
  owner: 'qemu/140',
  guest: { kind: 'qemu', vmid: 140, name: 'docs' },
  state: 'active',
  zone: 'example.com',
  accountId: account,
  service: 'http://10.0.0.140:8080',
  level: 'port',
  replicas: [],
}

export const firstActions: State['actions'] = [
  { kind: 'create-tunnel', credentialId: credential, accountId: account, target: tunnel, detail: `in account ${account}`, destructive: false, applied: false, held: 'observe mode' },
  {
    kind: 'put-config',
    credentialId: credential,
    accountId: account,
    target: tunnel,
    detail: `in account ${account}: 3 rules, first configuration`,
    destructive: false,
    applied: false,
    held: 'observe mode',
  },
  {
    kind: 'create-record',
    credentialId: credential,
    target: 'docs.example.com',
    detail: `in zone example.com: tunnel ${tunnel} has no known id`,
    destructive: false,
    applied: false,
    held: 'tunnel not created yet',
  },
]

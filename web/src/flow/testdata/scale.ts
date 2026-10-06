// States of many routes for the tests of the map, made from the populated
// golden as the fake daemon's scenarios large, outage and wide are: every
// route of a guest of its own unless asked otherwise, on vmbr0, or on vmbr1
// with VLAN 20 for every fourth guest.

import type { ConnectorStatus, RouteView, State, TrafficView, TunnelView, ZoneView } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'

const base = populated as unknown as State

export interface Down {
  state: string
  reason: string
}

export interface ScaleOptions {
  routes: number
  guests?: number // the routes are spread over this many guests
  zones?: number // example.com, then zone-1.example ...
  accounts?: number // the zones are spread over this many accounts, each with its tunnel
  down?: (i: number) => Down | undefined // the routes that are not active
}

const served = (name: string, account: string, n: number): ZoneView => ({
  name,
  id: `zone${n}`,
  status: 'active',
  accountId: account,
  state: 'served',
  credentials: ['cred1'],
  servedBy: 'cred1',
  stale: [],
  excluded: [],
})

const accountId = (a: number) => (a === 0 ? 'acc1' : `acc-${a}`)
const tunnelId = (a: number) => `00000000-0000-4000-8000-${String(1 + a).padStart(12, '0')}`

export function scaled(o: ScaleOptions): State {
  const accounts = o.accounts ?? 1
  const zones = Array.from({ length: o.zones ?? 1 }, (_, z) => served(z === 0 ? 'example.com' : `zone-${z}.example`, accountId(z % accounts), z))
  const tunnels: TunnelView[] = Array.from({ length: accounts }, (_, a) => ({
    accountId: accountId(a),
    credentialId: 'cred1',
    name: 'pco-abc123',
    id: tunnelId(a),
    version: 3,
    exists: true,
    verified: true,
    unknown: false,
    unchecked: false,
  }))
  const connectors: ConnectorStatus[] = tunnels.map((t) => ({ tunnelId: t.id ?? '', active: true, ready: true, connections: 4 }))
  const guests = o.guests ?? o.routes
  const perGuest = new Map<number, number>()
  const routes: RouteView[] = Array.from({ length: o.routes }, (_, i) => {
    const vmid = 1000 + Math.floor((i * guests) / o.routes)
    const nth = perGuest.get(vmid) ?? 0
    perGuest.set(vmid, nth + 1)
    const zone = zones[i % zones.length] ?? zones[0]
    const host = `app-${String(i).padStart(4, '0')}.${zone?.name ?? 'example.com'}`
    const addr = `10.${(vmid >> 8) & 255}.${vmid & 255}.10`
    const service = `http://${addr}:${8080 + nth}`
    const down = o.down?.(i)
    const state = down?.state ?? 'active'
    const answers = state === 'active' || (state === 'unreachable' && down?.reason === 'target is not answering')
    const r: RouteView = {
      hostname: host,
      owner: `qemu/${vmid}`,
      state,
      guest: { kind: 'qemu', vmid, name: `app-${vmid}` },
      candidates: [{ addr, source: 'static', ok: state === 'active', level: 'port' }],
      path: { node: 'pve1', bridge: vmid % 4 === 3 ? 'vmbr1' : 'vmbr0', ...(vmid % 4 === 3 ? { vlan: 20 } : {}), port: `tap${vmid}i0`, verifiedAt: '2026-10-01T12:00:00Z' },
    }
    if (state !== 'no-zone') {
      r.zone = zone?.name
      r.accountId = zone?.accountId
      r.rule = { hostname: host, service: answers ? service : 'http_status:503' }
    }
    if (answers) {
      r.service = service
      r.level = 'port'
    }
    if (down) r.reason = down.reason
    return r
  })
  return {
    ...base,
    digest: `scaled${o.routes}`,
    routes,
    tunnels,
    connectors,
    zones,
    unapproved: [],
    conflicts: [],
    rogueConnectors: [],
  }
}

// trafficFor gives every tunnel a rate and every active route a figure.
export function trafficFor(st: State, routesWhy?: string): TrafficView {
  const at = st.at ?? '2026-10-01T12:00:00Z'
  return {
    at,
    interval: '5s',
    tunnels: st.connectors.map((c, n) => ({
      tunnelId: c.tunnelId,
      node: 'pve1',
      configVersion: 3,
      haConnections: 4,
      edges: [],
      rttMs: [],
      stale: false,
      samples: [{ at, rps: 40 + n, errorsPerSec: n === 0 ? 0.5 : 0, concurrent: 2 }],
    })),
    routes: routesWhy
      ? []
      : st.routes
          .filter((r) => r.state === 'active' && r.service)
          .map((r, i) => ({ hostname: r.hostname, owner: r.owner, target: (r.service ?? '').replace(/^[a-z]+:\/\//, ''), flowsPerSec: (i % 7) * 0.5, stale: false })),
    routesTotal: 0,
    ...(routesWhy ? { routesWhy } : {}),
  }
}

const reasons = ['target is not answering', 'no verified address yet', 'address was verified for another owner']

// large is 1000 routes over 6 zones, 40 of them not active: unreachable,
// withdrawn and without a zone.
export function large(): State {
  return scaled({
    routes: 1000,
    zones: 6,
    down: (i) => {
      if (i >= 40) return undefined
      if (i % 5 < 2) return { state: 'unreachable', reason: 'target is not answering' }
      if (i % 5 === 2) return { state: 'withdrawn', reason: 'identity check failed' }
      if (i % 5 === 3) return { state: 'held', reason: 'named in the Notes but not routed; claim kept' }
      return { state: 'no-zone', reason: 'zone example.net is served through no credential' }
    },
  })
}

// outage is 1000 routes over 3 zones, 600 of them unreachable for one of
// three reasons.
export function outage(): State {
  return scaled({ routes: 1000, zones: 3, down: (i) => (i < 600 ? { state: 'unreachable', reason: reasons[Math.floor(i / 3) % 3] ?? '' } : undefined) })
}

// wide is 50 zones in 30 accounts, so 30 tunnels, and 300 routes.
export function wide(): State {
  return scaled({ routes: 300, zones: 50, accounts: 30 })
}

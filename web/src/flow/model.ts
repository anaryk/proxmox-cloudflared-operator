// The flow map's model: the daemon's state and traffic as the nodes and
// edges of five bands, hostnames to targets, and the same chains as a list.
// Nothing here knows where a node is drawn; layout.ts places them and
// collapse.ts folds them to fit.

import type { ConnectorStatus, RouteTraffic, RouteView, State, TrafficView, TunnelTraffic, TunnelView, ZoneView } from '../api/types.gen'
import { connectorText, verifiedText } from '../text/words'
import type { Band, FlowEdge, FlowNode, FlowPort, FlowRow, Model } from './types'

export type { Band, FlowEdge, FlowNode, FlowPort, FlowRow, Model } from './types'

// routeKey names a route: a hostname has one route per owner.
export const routeKey = (hostname: string, owner: string): string => `${hostname} ${owner}`

export const rowId = (key: string): string => `route:${key}`

export const noZone = 'no-zone'
export const noPath = 'path:none'

const bandOrder: readonly Band[] = ['hostnames', 'edge', 'connector', 'path', 'targets']

// The states of a route drawn past the connector to its target, and those
// with a rule in the tunnel: a held route has one, which answers 503.
const downstream = new Set(['active', 'unreachable', 'withdrawn', 'frozen'])
const ruled = new Set([...downstream, 'held'])

const unset = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')

// firstCycleDone says whether the daemon has finished a cycle: before one,
// the map and the tiles wait for it rather than show no routes.
export const firstCycleDone = (st: Pick<State, 'at'>): boolean => !unset(st.at)

// compareOwners orders owners as model.CompareOwners does: qemu, then lxc,
// each by VMID, then manual routes by id.
export function compareOwners(a: string, b: string): number {
  const key = (o: string): [number, number] => {
    const m = /^(qemu|lxc)\/(\d+)$/.exec(o)
    if (m) return [m[1] === 'qemu' ? 0 : 1, Number(m[2])]
    return [o.startsWith('manual/') ? 2 : 3, 0]
  }
  const [ra, va] = key(a)
  const [rb, vb] = key(b)
  return ra - rb || va - vb || (a < b ? -1 : a > b ? 1 : 0)
}

const byName = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

// Target is where a route's service points: an address and a port, which
// is what the egress filter counts.
export interface Target {
  scheme: string
  host: string
  port: string
  target: string // '10.0.0.11:8080', '[fd00::5]:443'
}

const defaultPorts: Readonly<Record<string, string>> = { http: '80', https: '443' }

export function targetOf(service?: string): Target | undefined {
  if (!service) return undefined
  let u: URL
  try {
    u = new URL(service)
  } catch {
    return undefined
  }
  const scheme = u.protocol.replace(/:$/, '')
  const port = u.port || defaultPorts[scheme]
  if (!u.hostname || !port) return undefined
  return { scheme, host: u.hostname, port, target: `${u.hostname}:${port}` }
}

// PathRef is the bridge and VLAN a route was proven on, or none.
export interface PathRef {
  id: string
  label: string
}

export function pathOf(r: Pick<RouteView, 'path'>): PathRef {
  const p = r.path
  if (!p?.bridge) return { id: noPath, label: 'no bridge proven' }
  return {
    id: `path:${p.bridge}${p.vlan ? `.${p.vlan}` : ''}`,
    label: `${p.bridge}${p.vlan ? ` · VLAN ${p.vlan}` : ''} · direct`,
  }
}

export const tunnelNodeId = (account: string): string => `edge:${account}`
export const connectorNodeId = (account: string): string => `connector:${account}`

function targetNode(r: RouteView, t: Target | undefined): { id: string; label: string; ref: string; lines: string[] } | undefined {
  if (r.guest) {
    const ref = `${r.guest.kind}/${r.guest.vmid}`
    return { id: `guest:${ref}`, label: r.guest.name || ref, ref, lines: [ref] }
  }
  if (t) return { id: `address:${t.host}`, label: t.host, ref: t.host, lines: [r.owner] }
  return undefined
}

// The worse of two states on one access point: a port some route cannot
// reach is drawn as one that cannot be reached.
const portRank: Readonly<Record<string, number>> = { unreachable: 0, withdrawn: 1, frozen: 2, active: 3 }
const worse = (a: string, b: string) => ((portRank[a] ?? 4) <= (portRank[b] ?? 4) ? a : b)

const lineOf = (state: string): FlowEdge['style'] => (state === 'unreachable' ? 'unreachable' : state === 'withdrawn' ? 'withdrawn' : 'plain')

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}

// What the state and the traffic say, indexed once for both the map and the
// chain list.
class Index {
  readonly names = new Map<string, string>()
  readonly zones = new Map<string, ZoneView>()
  readonly tunnels = new Map<string, TunnelView>()
  readonly connectors = new Map<string, ConnectorStatus>() // by account
  readonly traffic = new Map<string, TunnelTraffic>() // by tunnel id
  readonly figures = new Map<string, RouteTraffic>() // by route key
  readonly byTarget = new Map<string, RouteTraffic>()
  readonly shared = new Map<string, number>() // routes on each target
  readonly records = new Map<string, { type: string; content: string }>()
  readonly holders = new Map<string, RouteView>()

  constructor(
    readonly st: State,
    tv: TrafficView | undefined,
  ) {
    for (const c of st.credentials) for (const a of c.report?.accounts ?? []) if (!this.names.has(a.id)) this.names.set(a.id, a.name)
    for (const z of st.zones) this.zones.set(z.name, z)
    for (const t of st.tunnels) this.tunnels.set(t.accountId, t)
    const accountOf = new Map(st.tunnels.filter((t) => t.id).map((t) => [t.id, t.accountId]))
    for (const c of st.connectors) {
      const account = accountOf.get(c.tunnelId)
      if (account !== undefined && !this.connectors.has(account)) this.connectors.set(account, c)
    }
    for (const t of tv?.tunnels ?? []) this.traffic.set(t.tunnelId, t)
    if (tv && !tv.routesWhy) {
      for (const f of tv.routes) {
        this.figures.set(routeKey(f.hostname, f.owner), f)
        if (!this.byTarget.has(f.target)) this.byTarget.set(f.target, f)
      }
    }
    for (const c of st.conflicts) this.records.set(c.name, { type: c.type, content: c.content })
    for (const r of st.routes) {
      if (r.state !== 'conflict' && !this.holders.has(r.hostname)) this.holders.set(r.hostname, r)
      const t = downstream.has(r.state) ? targetOf(r.service) : undefined
      if (t) this.shared.set(t.target, (this.shared.get(t.target) ?? 0) + 1)
    }
  }

  accountName(account: string): string {
    return this.names.get(account) ?? account
  }

  // A tunnel is labelled by its account and the start of its id: every
  // tunnel of an install has the same name.
  tunnelLabel(t: TunnelView): string {
    return t.id ? `${this.accountName(t.accountId)} · ${t.id.slice(0, 8)}` : this.accountName(t.accountId)
  }

  // zoneOf is the zone a hostname is in: the route's own, else the longest
  // zone its name ends in.
  zoneOf(hostname: string, zone?: string): ZoneView | string | undefined {
    if (zone) return this.zones.get(zone) ?? zone
    let best: ZoneView | undefined
    for (const z of this.zones.values()) {
      if ((hostname === z.name || hostname.endsWith(`.${z.name}`)) && (!best || z.name.length > best.name.length)) best = z
    }
    return best
  }

  // accountOf is the account whose tunnel carries the route's rule.
  accountOf(r: RouteView): string | undefined {
    if (!ruled.has(r.state)) return undefined
    if (r.accountId && r.rule) return r.accountId
    if (r.state !== 'frozen') return undefined
    const z = this.zoneOf(r.hostname, r.zone)
    return r.accountId || (typeof z === 'object' ? z.accountId : undefined)
  }

  // figureOf is the connections opened to the route's target per second,
  // when the egress filter counts them.
  figureOf(r: RouteView, t: Target | undefined): RouteTraffic | undefined {
    return this.figures.get(routeKey(r.hostname, r.owner)) ?? (t ? this.byTarget.get(t.target) : undefined)
  }

  latest(tunnelId: string | undefined): { rps: number; errors: number; concurrent: number; stale: boolean } | undefined {
    const t = tunnelId ? this.traffic.get(tunnelId) : undefined
    const s = t?.samples.at(-1)
    if (!t || !s) return undefined
    return { rps: s.rps, errors: s.errorsPerSec, concurrent: s.concurrent, stale: t.stale }
  }

  holderOf(r: RouteView): string | undefined {
    if (r.state !== 'conflict') return undefined
    const h = this.holders.get(r.hostname)
    if (!h) return undefined
    return h.guest?.name ? `${h.owner} ${h.guest.name}` : h.owner
  }

  rowOf(r: RouteView): FlowRow {
    const key = routeKey(r.hostname, r.owner)
    const tags: string[] = []
    if (r.state === 'held') tags.push('503')
    if (this.records.has(r.hostname)) tags.push('DNS')
    const row: FlowRow = { id: rowId(key), kind: 'route', hostname: r.hostname, owner: r.owner, state: r.state, tags }
    if (r.guest?.name) row.guest = r.guest.name
    if (r.reason) row.reason = r.reason
    const holder = this.holderOf(r)
    if (holder) row.holder = holder
    return row
  }
}

// The lines of a tunnel's edge node: its connections to the edge and where
// they land, from the connector's own metrics when there are any.
function edgeLines(t: TunnelView, c: ConnectorStatus | undefined, tt: TunnelTraffic | undefined): string[] {
  const lines: string[] = []
  const connections = tt?.haConnections ?? c?.connections
  if (connections !== undefined) lines.push(plural(connections, 'connection'))
  if (tt && tt.edges.length > 0) {
    const at = new Map<string, number>()
    for (const e of tt.edges) at.set(e.location, (at.get(e.location) ?? 0) + 1)
    lines.push([...at].map(([loc, n]) => (n > 1 ? `${loc} ×${n}` : loc)).join(' · '))
  }
  const rtt = (tt?.rttMs ?? []).filter((v) => Number.isFinite(v))
  if (rtt.length > 0) {
    const lo = Math.round(Math.min(...rtt))
    const hi = Math.round(Math.max(...rtt))
    lines.push(lo === hi ? `RTT ${lo} ms` : `RTT ${lo}–${hi} ms`)
  }
  if (t.held) lines.push(t.held)
  return lines
}

function connectorLines(label: string, t: TunnelView, tt: TunnelTraffic | undefined): string[] {
  const lines = [label]
  if (tt) {
    lines.push(tt.configVersion === t.version ? `config v${tt.configVersion} · rolled out` : `config v${tt.configVersion} · v${t.version} rolling out`)
    if (tt.cloudflared) lines.push(`cloudflared ${tt.cloudflared}`)
  } else if (t.rollout) {
    lines.push(`version ${t.rollout.version} runs on ${plural(t.rollout.connectors, 'connector')}`)
  }
  return lines
}

interface PortAcc {
  port: FlowPort
  routes: string[]
}

interface TargetAcc {
  node: FlowNode
  ports: Map<string, PortAcc>
  routes: Set<string>
}

interface EdgeAcc {
  edge: FlowEdge
  routes: Set<string>
  figures: Map<string, RouteTraffic> // by target, each once
  muted: boolean[]
}

// buildModel is the map of the state: a zone card per zone with a row per
// hostname, an edge node per tunnel, a node per connector and per connector
// pco does not run, a node per bridge and VLAN routes were proven on, and a
// card per target with an access point per port. Hostname edges never move;
// the trunk carries the tunnel's request rate, the edges past the connector
// the connections opened to their targets.
export function buildModel(st: State, traffic: TrafficView | undefined): Model {
  if (!firstCycleDone(st)) return { nodes: [], edges: [] }
  const ix = new Index(st, traffic)
  const nodes = new Map<string, FlowNode>()
  const edges = new Map<string, EdgeAcc>()
  const targets = new Map<string, TargetAcc>()
  const touched = (id: string, key: string) => {
    const n = nodes.get(id)
    if (n) (n.routes ??= []).push(key)
  }
  const link = (edge: FlowEdge, key?: string, f?: RouteTraffic, muted = false) => {
    let e = edges.get(edge.id)
    if (!e) {
      e = { edge, routes: new Set(), figures: new Map(), muted: [] }
      edges.set(edge.id, e)
    }
    if (key) e.routes.add(key)
    if (f) e.figures.set(f.target, f)
    e.muted.push(muted)
  }

  // The tunnels, their connectors and the connectors pco does not run.
  for (const t of [...st.tunnels].sort((a, b) => byName(a.accountId, b.accountId))) {
    const c = ix.connectors.get(t.accountId)
    const tt = t.id ? ix.traffic.get(t.id) : undefined
    const label = ix.tunnelLabel(t)
    const edgeId = tunnelNodeId(t.accountId)
    const lines = edgeLines(t, c, tt)
    nodes.set(edgeId, { id: edgeId, band: 'edge', kind: 'edge', label, state: verifiedText(t), ref: t.accountId, ...(lines.length > 0 ? { lines } : {}) })
    if (!c) continue
    const id = connectorNodeId(t.accountId)
    const node: FlowNode = { id, band: 'connector', kind: 'connector', label: st.node || 'this node', state: connectorText(c), ref: t.accountId, lines: connectorLines(label, t, tt) }
    if (t.unchecked) node.tags = ['unchecked']
    nodes.set(id, node)
    const now = ix.latest(c.tunnelId)
    const trunk: FlowEdge = { id: `${edgeId}>${id}`, from: edgeId, to: id, style: 'trunk', lanes: Math.min(4, Math.max(1, tt?.haConnections ?? c.connections)) }
    if (now) {
      trunk.rate = now.rps
      trunk.errors = now.errors
      if (now.stale) trunk.stale = true
    }
    if (t.unchecked || now?.stale) trunk.muted = true
    link(trunk)
  }
  for (const r of [...st.rogueConnectors].sort((a, b) => byName(a.id, b.id))) {
    const id = `rogue:${r.id}`
    const t = ix.tunnels.get(r.accountId)
    const lines = [r.version ? `cloudflared ${r.version}` : 'version not known']
    if (t) lines.push(ix.tunnelLabel(t))
    nodes.set(id, { id, band: 'connector', kind: 'rogue', label: r.originIp ? `not run by pco: ${r.originIp}` : 'not run by pco', state: 'rogue', ref: r.id, lines })
    const edgeId = tunnelNodeId(r.accountId)
    if (nodes.has(edgeId)) link({ id: `${edgeId}>${id}`, from: edgeId, to: id, style: 'rogue' })
  }

  // The hostnames, by zone card.
  const cards = new Map<string, FlowNode>()
  const cardFor = (zone: ZoneView | string | undefined): FlowNode => {
    if (zone === undefined) {
      let n = cards.get(noZone)
      if (!n) cards.set(noZone, (n = { id: noZone, band: 'hostnames', kind: 'zone', label: 'No zone', rows: [] }))
      return n
    }
    const name = typeof zone === 'string' ? zone : zone.name
    const id = `zone:${name}`
    let n = cards.get(id)
    if (!n) {
      n = { id, band: 'hostnames', kind: 'zone', label: name, ref: name, rows: [] }
      if (typeof zone === 'object') {
        n.state = zone.state
        n.lines = [ix.accountName(zone.accountId)]
      }
      cards.set(id, n)
    }
    return n
  }

  const routes = [...st.routes].sort((a, b) => byName(a.hostname, b.hostname) || compareOwners(a.owner, b.owner))
  for (const r of routes) {
    const key = routeKey(r.hostname, r.owner)
    const card = cardFor(r.state === 'no-zone' ? undefined : ix.zoneOf(r.hostname, r.zone))
    card.rows?.push(ix.rowOf(r))
    ;(card.routes ??= []).push(key)
    const account = ix.accountOf(r)
    if (account === undefined) continue
    const t = ix.tunnels.get(account)
    const muted = r.state === 'frozen' || !!t?.unchecked
    const edgeId = tunnelNodeId(account)
    if (!nodes.has(edgeId)) {
      nodes.set(edgeId, { id: edgeId, band: 'edge', kind: 'edge', label: ix.accountName(account), ref: account })
    }
    link({ id: `${card.id}>${edgeId}`, from: card.id, to: edgeId, style: 'hairline' }, key, undefined, r.state === 'frozen')
    touched(edgeId, key)
    const connectorId = connectorNodeId(account)
    const trunk = edges.get(`${edgeId}>${connectorId}`)
    if (trunk) {
      trunk.routes.add(key)
      touched(connectorId, key)
    }
    if (!downstream.has(r.state)) continue

    const t2 = targetOf(r.service)
    const tn = targetNode(r, t2)
    if (!tn) continue
    const path = pathOf(r)
    if (!nodes.has(path.id)) nodes.set(path.id, { id: path.id, band: 'path', kind: 'path', label: path.label, ref: path.id })
    touched(path.id, key)
    let target = targets.get(tn.id)
    if (!target) {
      target = { node: { id: tn.id, band: 'targets', kind: 'target', label: tn.label, ref: tn.ref, lines: tn.lines }, ports: new Map(), routes: new Set() }
      targets.set(tn.id, target)
    }
    target.routes.add(key)
    const f = ix.figureOf(r, t2)
    if (trunk) link({ id: `${connectorId}>${path.id}`, from: connectorId, to: path.id, style: 'plain' }, key, t2 ? f : undefined, muted)

    if (t2) {
      const portId = `${tn.id}|${t2.target}`
      let p = target.ports.get(portId)
      if (!p) {
        p = { port: { id: portId, label: `:${t2.port} ${t2.scheme}`, state: r.state, target: t2.target }, routes: [] }
        target.ports.set(portId, p)
      }
      p.port.state = worse(p.port.state, r.state)
      if (r.level && !p.port.level) p.port.level = r.level
      if (f) {
        p.port.rate = f.flowsPerSec
        if (f.stale) p.port.stale = true
      }
      p.routes.push(key)
      link({ id: `${path.id}>${portId}`, from: path.id, to: tn.id, port: portId, style: 'plain' }, key, f, muted)
    } else {
      // No address passed: the edge ends at the card, not at a port.
      const style = lineOf(r.state)
      link({ id: `${path.id}>${tn.id}#${style}`, from: path.id, to: tn.id, style, ...(style === 'withdrawn' ? { label: '503' } : {}) }, key, undefined, muted)
    }
  }

  // Guests waiting for approval ask for hostnames too; they have no route.
  for (const g of st.unapproved) {
    const owner = `${g.kind}/${g.vmid}`
    for (const hostname of g.hostnames) {
      const key = routeKey(hostname, owner)
      const card = cardFor(ix.zoneOf(hostname))
      const row: FlowRow = { id: rowId(key), kind: 'unapproved', hostname, owner, state: 'unapproved', tags: ['waits for approval'] }
      if (g.name) row.guest = g.name
      if (g.why.length > 0) row.reason = g.why.join('; ')
      card.rows?.push(row)
      ;(card.routes ??= []).push(key)
    }
  }

  for (const card of cards.values()) {
    card.rows?.sort((a, b) => byName(a.hostname, b.hostname) || compareOwners(a.owner, b.owner))
    nodes.set(card.id, card)
  }
  for (const t of targets.values()) {
    const ports = [...t.ports.values()].sort((a, b) => Number(a.port.target?.split(':').at(-1)) - Number(b.port.target?.split(':').at(-1)) || byName(a.port.id, b.port.id))
    if (ports.length > 0) t.node.ports = ports.map((p) => ({ ...p.port, routes: p.routes }))
    t.node.routes = [...t.routes]
    nodes.set(t.node.id, t.node)
  }

  const out: FlowEdge[] = []
  for (const { edge, routes: keys, figures, muted } of edges.values()) {
    const e: FlowEdge = { ...edge }
    const port = e.port ? targets.get(e.to)?.ports.get(e.port) : undefined
    if (port) e.style = lineOf(port.port.state)
    if (keys.size > 0) e.routes = [...keys]
    if (e.style === 'hairline') e.label = plural(keys.size, 'rule')
    if (e.style !== 'trunk' && e.style !== 'rogue' && muted.length > 0 && muted.every(Boolean)) e.muted = true
    if (e.style !== 'trunk' && e.style !== 'hairline' && figures.size > 0) {
      const fs = [...figures.values()]
      e.rate = fs.reduce((sum, f) => sum + f.flowsPerSec, 0)
      if (fs.every((f) => f.stale)) e.stale = true
    }
    out.push(e)
  }

  const order = (n: FlowNode) => bandOrder.indexOf(n.band)
  return {
    nodes: [...nodes.values()].sort((a, b) => order(a) - order(b) || byName(a.id, b.id)),
    edges: out.sort((a, b) => byName(a.id, b.id)),
  }
}

// The chain list: a route as the steps of its chain, in words.

export interface ChainStep {
  key: string
  name: string
  level: 'ok' | 'warn' | 'fail' | 'info' | 'skipped'
  text: string
}

export interface Chain {
  key: string
  hostname: string
  owner: string
  guest?: string
  state: string // a route's state, or 'unapproved'
  reason?: string
  tags: string[]
  steps: ChainStep[]
  // The connections opened to the route's target, when they are counted.
  figure?: { target: string; rate: number; shared: number; stale: boolean }
}

function zoneStep(ix: Index, r: { hostname: string; zone?: string; state: string; reason?: string }): ChainStep {
  if (r.state === 'no-zone') return { key: 'zone', name: 'Zone', level: 'fail', text: r.reason || 'no zone of a credential holds this name' }
  const z = ix.zoneOf(r.hostname, r.zone)
  if (z === undefined) return { key: 'zone', name: 'Zone', level: 'fail', text: 'no zone of a credential holds this name' }
  if (typeof z === 'string') return { key: 'zone', name: 'Zone', level: 'info', text: z }
  const account = ix.accountName(z.accountId)
  if (z.state === 'served') return { key: 'zone', name: 'Zone', level: 'ok', text: `${z.name} · served · ${account}` }
  if (z.state === 'frozen') return { key: 'zone', name: 'Zone', level: 'warn', text: `${z.name} · frozen: ${z.frozenWhy ?? ''}` }
  return { key: 'zone', name: 'Zone', level: 'fail', text: `${z.name} · ${z.state} · ${account}` }
}

function routeChain(ix: Index, r: RouteView): Chain {
  const row = ix.rowOf(r)
  const chain: Chain = { key: routeKey(r.hostname, r.owner), hostname: r.hostname, owner: r.owner, state: r.state, tags: row.tags, steps: [zoneStep(ix, r)] }
  if (row.guest) chain.guest = row.guest
  if (r.reason) chain.reason = r.reason
  const steps = chain.steps
  switch (r.state) {
    case 'no-zone':
      return chain
    case 'conflict':
      steps.push({ key: 'claim', name: 'Claim', level: 'fail', text: r.reason || 'another owner holds the hostname' })
      return chain
    case 'rejected':
      steps.push({ key: 'policy', name: 'Hostname policy', level: 'warn', text: r.reason || 'not published' })
      return chain
  }
  const record = ix.records.get(r.hostname)
  if (record) steps.push({ key: 'dns', name: 'DNS', level: 'fail', text: `a record of someone else holds the name: ${record.type} ${record.content}` })
  const account = ix.accountOf(r)
  const t = account === undefined ? undefined : ix.tunnels.get(account)
  if (!t) {
    steps.push({ key: 'edge', name: 'Edge', level: 'fail', text: account === undefined ? 'no tunnel carries this name' : `no tunnel in account ${ix.accountName(account)}` })
    return chain
  }
  const verified = verifiedText(t)
  steps.push({ key: 'edge', name: 'Edge', level: verified === 'yes' ? 'ok' : 'warn', text: `${ix.tunnelLabel(t)} · verified: ${verified}${t.held ? `: ${t.held}` : ''}` })
  const c = ix.connectors.get(t.accountId)
  if (!c) {
    steps.push({ key: 'connector', name: 'Connector', level: 'fail', text: 'no connector on this node' })
  } else {
    const level = !c.active ? 'fail' : c.ready && !c.tokenRefused && !c.metricsPortHeld ? 'ok' : 'warn'
    steps.push({ key: 'connector', name: 'Connector', level, text: `${ix.st.node || 'this node'} · ${connectorText(c)}` })
  }
  if (r.state === 'held' || r.state === 'frozen') {
    steps.push({ key: 'rule', name: 'Rule', level: 'warn', text: r.state === 'held' ? `503: ${r.reason ?? 'served by nobody'}` : (r.reason ?? 'account frozen') })
    return chain
  }
  const path = pathOf(r)
  steps.push({ key: 'path', name: 'Path', level: path.id === noPath ? 'info' : 'ok', text: path.label })
  const t2 = targetOf(r.service)
  const who = r.guest ? `${r.owner}${r.guest.name ? ` ${r.guest.name}` : ''}` : r.owner
  const where = t2 ? `${who} · ${t2.target} ${t2.scheme}` : who
  if (r.state === 'active') steps.push({ key: 'target', name: 'Target', level: 'ok', text: r.level ? `${where} · level ${r.level}` : where })
  else if (r.state === 'withdrawn') steps.push({ key: 'target', name: 'Target', level: 'warn', text: `${where} · withdrawn (503): ${r.reason ?? ''}` })
  else steps.push({ key: 'target', name: 'Target', level: 'fail', text: `${where} · ${r.reason ?? r.state}` })
  const f = t2 ? ix.figureOf(r, t2) : undefined
  if (f && t2) chain.figure = { target: t2.target, rate: f.flowsPerSec, shared: ix.shared.get(t2.target) ?? 1, stale: f.stale }
  return chain
}

// buildChains is every hostname of the state as its chain, ordered by
// hostname and owner: the chain list, and the map's text alternative.
export function buildChains(st: State, traffic: TrafficView | undefined): Chain[] {
  if (!firstCycleDone(st)) return []
  const ix = new Index(st, traffic)
  const out = st.routes.map((r) => routeChain(ix, r))
  for (const g of st.unapproved) {
    const owner = `${g.kind}/${g.vmid}`
    for (const hostname of g.hostnames) {
      const chain: Chain = {
        key: routeKey(hostname, owner),
        hostname,
        owner,
        state: 'unapproved',
        tags: ['waits for approval'],
        steps: [zoneStep(ix, { hostname, state: 'unapproved' }), { key: 'approval', name: 'Approval', level: 'info', text: `waits for approval${g.why.length > 0 ? `: ${g.why.join('; ')}` : ''}` }],
      }
      if (g.name) chain.guest = g.name
      out.push(chain)
    }
  }
  return out.sort((a, b) => byName(a.hostname, b.hostname) || compareOwners(a.owner, b.owner))
}

// Trunk is a tunnel's figures as the chain list's strip and the tiles show
// them.
export interface Trunk {
  account: string
  label: string
  rps?: number
  errors?: number
  concurrent?: number
  stale: boolean
  ready: boolean
}

export function trunksOf(st: State, traffic: TrafficView | undefined): Trunk[] {
  const ix = new Index(st, traffic)
  const out: Trunk[] = []
  for (const t of [...st.tunnels].sort((a, b) => byName(a.accountId, b.accountId))) {
    const c = ix.connectors.get(t.accountId)
    if (!c) continue
    const now = ix.latest(c.tunnelId)
    const trunk: Trunk = { account: t.accountId, label: ix.tunnelLabel(t), stale: now?.stale ?? false, ready: c.active && c.ready }
    if (now) {
      trunk.rps = now.rps
      trunk.errors = now.errors
      trunk.concurrent = now.concurrent
    }
    out.push(trunk)
  }
  return out
}

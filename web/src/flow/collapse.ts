// Folding the map to fit what a browser draws smoothly: whatever the install,
// at most 150 cards and 400 edges. A small install shows everything; a
// larger one folds its zone cards and its guests whose routes are all
// active; a very large one shows counts and its problems. A focus shows one
// chain or one neighbourhood whole.

import { noPath, routeKey } from './model'
import type { FlowEdge, FlowNode, FlowRow, Model } from './types'

export type Level = 'full' | 'folded' | 'collapsed'

// The render budget. The rows are those the budget's cards were measured
// with; past them the problems of a collapsed map are grouped.
export const budget = { cards: 150, edges: 400, rows: 240 }

// A folded zone card shows this many rows, problems first.
export const foldedRows = 12

// A level is entered above its limit and left only 10 % below it, so a map
// near a limit does not switch on every cycle.
const limits = { folded: 40, collapsed: 200 }
const leaveAt = 0.9

const levels: readonly Level[] = ['full', 'folded', 'collapsed']
const rank = (l: Level) => levels.indexOf(l)
const higher = (a: Level, b: Level): Level => (rank(a) >= rank(b) ? a : b)

// levelOf is the level for n routes, or n guests, after the level before.
export function levelOf(n: number, previous?: Level): Level {
  const fresh: Level = n > limits.collapsed ? 'collapsed' : n > limits.folded ? 'folded' : 'full'
  if (!previous || rank(fresh) >= rank(previous)) return fresh
  if (previous === 'collapsed' && n >= limits.collapsed * leaveAt) return 'collapsed'
  if (n >= limits.folded * leaveAt) return 'folded'
  return fresh
}

// Expanding this opens every folded card and group.
export const expandAll = '*'

export const moreId = (zone: string) => `more:${zone}`

const isRoute = (r: FlowRow) => r.kind === undefined || r.kind === 'route' || r.kind === 'unapproved'
const keyOf = (r: FlowRow) => routeKey(r.hostname, r.owner)
const isProblem = (r: FlowRow) => r.kind === 'unapproved' || (isRoute(r) && r.state !== 'active')

const byName = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

function routeRows(m: Model): FlowRow[] {
  return m.nodes.flatMap((n) => (n.kind === 'zone' ? (n.rows ?? []).filter(isRoute) : []))
}

// The size of a view: its hostnames, and the guests it draws (not every
// guest Proxmox lists).
function sizeOf(m: Model): { routes: number; guests: number } {
  return { routes: routeRows(m).length, guests: m.nodes.filter((n) => n.kind === 'target').length }
}

// mapLevel is the level of a map, after the level before: the higher of
// the levels of its hostnames and of its guests.
export function mapLevel(m: Model, previous?: Level): Level {
  const size = sizeOf(m)
  return higher(levelOf(size.routes, previous), levelOf(size.guests, previous))
}

// Focus: a hostname, a guest, a zone or a tunnel, as the address bar has it
// (hostname:www.example.com, guest:qemu/101, zone:example.com,
// tunnel:<account>), a node or row of the map by its id, or else words to
// look for in the hostnames, owners and guest names.
export function focusOn(m: Model, focus: string): { routes: Set<string>; nodes: Set<string> } {
  const routes = new Set<string>()
  const nodes = new Set<string>()
  const byId = new Map(m.nodes.map((n) => [n.id, n]))
  const ofNode = (id: string) => {
    const n = byId.get(id)
    if (!n) return
    nodes.add(id)
    for (const k of n.routes ?? []) routes.add(k)
  }
  const rows = routeRows(m)
  const [kind, ...rest] = focus.split(':')
  const value = rest.join(':')
  if (kind === 'hostname') {
    for (const r of rows) if (r.hostname === value) routes.add(keyOf(r))
  } else if (kind === 'guest') {
    for (const r of rows) if (r.owner === value) routes.add(keyOf(r))
    ofNode(focus)
  } else if (kind === 'tunnel') {
    ofNode(`edge:${value}`)
    ofNode(`connector:${value}`)
    for (const n of m.nodes) if (n.kind === 'rogue' && m.edges.some((e) => e.from === `edge:${value}` && e.to === n.id)) nodes.add(n.id)
  } else if (kind === 'route') {
    for (const r of rows) if (r.id === focus) routes.add(keyOf(r))
  } else if (byId.has(focus)) {
    ofNode(focus)
  } else {
    const words = focus.trim().toLowerCase()
    if (words) {
      for (const r of rows) {
        if ([r.hostname, r.owner, r.guest ?? ''].some((v) => v.toLowerCase().includes(words))) routes.add(keyOf(r))
      }
    }
  }
  return { routes, nodes }
}

// focusRoutes is the routes a focus keeps, for the chain list.
export function focusRoutes(m: Model, focus: string): Set<string> {
  return focusOn(m, focus).routes
}

// routesOf is the routes a node or a row of a view stands for: opening a
// group of problems lists them in the chain list rather than on the map.
export function routesOf(m: Model, id: string): string[] {
  for (const n of m.nodes) {
    if (n.id === id) return n.routes ?? []
    const row = n.rows?.find((r) => r.id === id)
    if (row) return row.routes ?? (isRoute(row) ? [keyOf(row)] : [])
  }
  return []
}

const keep = (list: readonly string[] | undefined, routes: ReadonlySet<string>) => (list ?? []).filter((k) => routes.has(k))

function rulesLabel(n: number): string {
  return n === 1 ? '1 rule' : `${n} rules`
}

// neighbourhood is the part of the map a focus keeps: the chains of its
// routes, and the nodes it names.
function neighbourhood(m: Model, focus: string): Model {
  const { routes, nodes: named } = focusOn(m, focus)
  const edges: FlowEdge[] = []
  for (const e of m.edges) {
    const carried = keep(e.routes, routes)
    if (carried.length > 0) {
      const kept: FlowEdge = { ...e, routes: carried }
      if (e.style === 'hairline') kept.label = rulesLabel(carried.length)
      edges.push(kept)
    } else if (named.has(e.from) && named.has(e.to)) {
      edges.push({ ...e, routes: [] })
    }
  }
  const ends = new Set(edges.flatMap((e) => [e.from, e.to]))
  const nodes: FlowNode[] = []
  for (const n of m.nodes) {
    const rows = n.rows?.filter((r) => routes.has(keyOf(r)))
    const has = named.has(n.id) || ends.has(n.id) || (rows !== undefined && rows.length > 0)
    if (!has) continue
    const kept: FlowNode = { ...n, routes: keep(n.routes, routes) }
    if (rows) kept.rows = rows
    if (n.ports) kept.ports = n.ports.filter((p) => keep(p.routes, routes).length > 0)
    nodes.push(kept)
  }
  return { nodes, edges }
}

function sortRows(rows: readonly FlowRow[]): FlowRow[] {
  return [...rows].sort((a, b) => Number(isProblem(b)) - Number(isProblem(a)))
}

function moreRow(zone: string, rest: readonly FlowRow[]): FlowRow {
  const states = new Set(rest.map((r) => r.state))
  const [only] = states
  const label = states.size === 1 && only === 'active' ? `+ ${rest.length} active` : `+ ${rest.length} more`
  return { id: moreId(zone), kind: 'more', hostname: '', owner: '', state: states.size === 1 && only ? only : 'more', tags: [], label, routes: rest.map(keyOf) }
}

// foldCard keeps the first rows of a zone card, problems first, and folds
// the rest into one row the user can open.
function foldCard(n: FlowNode, shown: (rows: FlowRow[]) => FlowRow[], expanded: ReadonlySet<string>): FlowNode {
  const rows = sortRows(n.rows ?? [])
  if (expanded.has(expandAll) || expanded.has(moreId(n.id))) return { ...n, rows }
  const show = shown(rows)
  const shownSet = new Set(show)
  const rest = rows.filter((r) => !shownSet.has(r))
  return { ...n, rows: rest.length > 0 ? [...show, moreRow(n.id, rest)] : show }
}

function countsOf(rows: readonly FlowRow[]): Record<string, number> {
  const counts: Record<string, number> = {}
  for (const r of rows) if (isRoute(r)) counts[r.state] = (counts[r.state] ?? 0) + 1
  return counts
}

// The state a target card is drawn in: the worst of its routes'.
const stateRank: Readonly<Record<string, number>> = { unreachable: 0, withdrawn: 1, frozen: 2, active: 3 }
const worstOf = (states: Iterable<string>) => {
  let worst = 'active'
  for (const s of states) if ((stateRank[s] ?? 4) < (stateRank[worst] ?? 4)) worst = s
  return worst
}

const styleOf = (state: string): FlowEdge['style'] => (state === 'unreachable' ? 'unreachable' : state === 'withdrawn' ? 'withdrawn' : 'plain')

export const guestsId = (path: string, state: string) => `guests:${path}|${state}`

// foldTargets folds the target cards chosen into one card per path and
// state ("38 guests on vmbr0"), with one edge from each path to it. A group
// the user opened stays open, and a group of one is the card itself.
function foldTargets(m: Model, chosen: (n: FlowNode, state: string) => boolean, expanded: ReadonlySet<string>): Model {
  const byId = new Map(m.nodes.map((n) => [n.id, n]))
  const pathOfTarget = new Map<string, string>()
  const statesOf = new Map<string, string[]>()
  for (const e of m.edges) {
    if (byId.get(e.to)?.kind !== 'target' || byId.get(e.from)?.kind !== 'path') continue
    if (!pathOfTarget.has(e.to)) pathOfTarget.set(e.to, e.from)
    const states = statesOf.get(e.to) ?? []
    states.push(e.style === 'unreachable' || e.style === 'withdrawn' ? e.style : e.muted ? 'frozen' : 'active')
    statesOf.set(e.to, states)
  }
  const groups = new Map<string, FlowNode[]>()
  for (const n of m.nodes) {
    if (n.kind !== 'target') continue
    const path = pathOfTarget.get(n.id) ?? noPath
    const state = worstOf([...(statesOf.get(n.id) ?? []), ...(n.ports ?? []).map((p) => p.state)])
    if (!chosen(n, state)) continue
    const id = guestsId(path, state)
    if (expanded.has(expandAll) || expanded.has(id)) continue
    groups.set(id, [...(groups.get(id) ?? []), n])
  }
  const into = new Map<string, string>()
  const added: FlowNode[] = []
  for (const [id, members] of groups) {
    if (members.length < 2) continue
    const [path = noPath, state = 'active'] = id.slice('guests:'.length).split('|')
    const where = byId.get(path)?.label.replace(/ · direct$/, '') ?? path
    const what = members.every((n) => n.id.startsWith('guest:')) ? 'guests' : 'targets'
    const routes = members.flatMap((n) => n.routes ?? [])
    added.push({
      id,
      band: 'targets',
      kind: 'group',
      label: path === noPath ? `${members.length} ${what}, no bridge proven` : `${members.length} ${what} on ${where}`,
      state,
      ref: path,
      routes,
    })
    for (const n of members) into.set(n.id, id)
  }
  if (into.size === 0) return m

  const edges: FlowEdge[] = []
  const merged = new Map<string, { e: FlowEdge; rates: Map<string, number>; stale: boolean[]; muted: boolean[]; routes: Set<string> }>()
  for (const e of m.edges) {
    const group = into.get(e.to)
    if (!group) {
      edges.push(e)
      continue
    }
    const id = `${e.from}>${group}`
    let g = merged.get(id)
    if (!g) {
      const state = byId.get(group)?.state ?? added.find((n) => n.id === group)?.state ?? 'active'
      g = { e: { id, from: e.from, to: group, style: styleOf(state) }, rates: new Map(), stale: [], muted: [], routes: new Set() }
      merged.set(id, g)
    }
    if (e.rate !== undefined) g.rates.set(e.port ?? e.id, e.rate)
    g.stale.push(!!e.stale)
    g.muted.push(!!e.muted)
    for (const k of e.routes ?? []) g.routes.add(k)
  }
  for (const { e, rates, stale, muted, routes } of merged.values()) {
    const out: FlowEdge = { ...e, routes: [...routes] }
    if (rates.size > 0) out.rate = [...rates.values()].reduce((a, b) => a + b, 0)
    if (stale.length > 0 && stale.every(Boolean) && rates.size > 0) out.stale = true
    if (muted.length > 0 && muted.every(Boolean)) out.muted = true
    if (out.style === 'withdrawn') out.label = '503'
    edges.push(out)
  }
  const nodes = [...m.nodes.filter((n) => !into.has(n.id)), ...added]
  return { nodes, edges: edges.sort((a, b) => byName(a.id, b.id)) }
}

// shape is the default view of a level.
function shape(m: Model, level: Level, problems: boolean, expanded: ReadonlySet<string>): Model {
  if (level === 'full') {
    if (!problems) return m
    return { ...m, nodes: m.nodes.map((n) => (n.kind === 'zone' && n.rows ? { ...n, rows: sortRows(n.rows) } : n)) }
  }
  let nodes: FlowNode[]
  if (level === 'folded') {
    nodes = m.nodes.map((n) => (n.kind === 'zone' ? foldCard(n, (rows) => rows.slice(0, foldedRows), expanded) : n))
  } else {
    nodes = m.nodes.map((n) => {
      if (n.kind !== 'zone') return n
      const card = foldCard(n, (rows) => (problems ? rows.filter(isProblem) : []), expanded)
      return { ...card, counts: countsOf(n.rows ?? []) }
    })
  }
  const folded = { ...m, nodes }
  if (level === 'collapsed' && !problems) return foldTargets(folded, () => true, expanded)
  return foldTargets(folded, (_, state) => state === 'active', expanded)
}

function cards(m: Model) {
  return m.nodes.length
}

function problemRows(m: Model) {
  return m.nodes.reduce((sum, n) => sum + (n.rows ?? []).filter(isProblem).length, 0)
}

const over = (m: Model) => cards(m) > budget.cards || m.edges.length > budget.edges

// aggregate groups the problems by zone, state and reason ("214
// unreachable: no answer on port 8080") and folds the guests behind them:
// opening a group lists its routes, it does not draw them.
function aggregate(m: Model): Model {
  const grouped = new Set<string>()
  const nodes = m.nodes.map((n) => {
    if (n.kind !== 'zone' || !n.rows) return n
    const groups = new Map<string, FlowRow[]>()
    for (const r of n.rows) {
      if (!isProblem(r)) continue
      const id = `group:${n.id}|${r.state}|${r.reason ?? ''}`
      groups.set(id, [...(groups.get(id) ?? []), r])
    }
    const rows: FlowRow[] = []
    const done = new Set<string>()
    for (const r of n.rows) {
      const id = `group:${n.id}|${r.state}|${r.reason ?? ''}`
      const members = isProblem(r) ? groups.get(id) : undefined
      if (!members || members.length < 2) {
        rows.push(r)
        continue
      }
      if (done.has(id)) continue
      done.add(id)
      for (const x of members) grouped.add(keyOf(x))
      const what = r.state === 'unapproved' ? 'wait for approval' : r.state
      const row: FlowRow = { id, kind: 'group', hostname: '', owner: '', state: r.state, tags: [], label: `${members.length} ${what}${r.reason ? `: ${r.reason}` : ''}`, routes: members.map(keyOf) }
      if (r.reason) row.reason = r.reason
      rows.push(row)
    }
    return { ...n, rows }
  })
  return foldTargets({ ...m, nodes }, (n) => (n.routes ?? []).length > 0 && (n.routes ?? []).every((k) => grouped.has(k)), new Set())
}

// foldZones folds the zone cards with the fewest problems into one card
// until the map fits, the last resort of an install with very many zones.
function foldZones(m: Model): Model {
  const zones = m.nodes.filter((n) => n.kind === 'zone')
  if (zones.length < 2) return m
  const order = [...zones].sort((a, b) => problemCount(a) - problemCount(b) || byName(a.id, b.id))
  for (let k = Math.max(2, cards(m) - budget.cards + 1); ; k = Math.min(zones.length, k * 2)) {
    const out = foldZoneSet(m, new Set(order.slice(0, k).map((n) => n.id)))
    if (!over(out) || k >= zones.length) return out
  }
}

function foldZoneSet(m: Model, folded: ReadonlySet<string>): Model {
  const zones = m.nodes.filter((n) => n.kind === 'zone')
  const id = 'zones:more'
  const members = zones.filter((n) => folded.has(n.id))
  const counts: Record<string, number> = {}
  for (const n of members) {
    for (const [state, c] of Object.entries(n.counts ?? countsOf(n.rows ?? []))) counts[state] = (counts[state] ?? 0) + c
  }
  const group: FlowNode = { id, band: 'hostnames', kind: 'group', label: `${members.length} more zones`, counts, routes: members.flatMap((n) => n.routes ?? []) }
  const edges: FlowEdge[] = []
  const merged = new Map<string, FlowEdge>()
  for (const e of m.edges) {
    if (!folded.has(e.from)) {
      edges.push(e)
      continue
    }
    const mid = `${id}>${e.to}`
    const was = merged.get(mid)
    const routes = [...(was?.routes ?? []), ...(e.routes ?? [])]
    const next: FlowEdge = { id: mid, from: id, to: e.to, style: e.style, routes, label: rulesLabel(routes.length) }
    if ((was ? was.muted : true) && e.muted) next.muted = true
    merged.set(mid, next)
  }
  return { nodes: [...m.nodes.filter((n) => !folded.has(n.id)), group], edges: [...edges, ...merged.values()].sort((a, b) => byName(a.id, b.id)) }
}

function problemCount(n: FlowNode): number {
  return (n.rows ?? []).filter(isProblem).length + (n.rows ?? []).filter((r) => r.kind === 'group').length
}

// fit keeps the map within its budget: the guests whose routes are all
// active folded again whatever was opened, then the problems grouped, then
// every guest folded, then the zone cards.
function fit(m: Model): Model {
  let out = m
  if (over(out)) out = foldTargets(out, (_, state) => state === 'active', new Set())
  if (over(out) || problemRows(out) > budget.rows) out = aggregate(out)
  if (over(out)) out = foldTargets(out, () => true, new Set())
  if (over(out)) out = foldZones(out)
  return out
}

export interface CollapseOptions {
  focus?: string
  expanded: ReadonlySet<string>
  previousLevel?: Level
  // Problems first: the routes that are not active stay drawn with their
  // chains. On by default when the map is collapsed.
  problems?: boolean
}

// collapse is the view of the map: the level its size gives, after the
// level before, and the model folded to that level and to the budget. The
// level is that of the whole map; a focus shows its neighbourhood at the
// level the neighbourhood's own size gives.
export function collapse(model: Model, opts: CollapseOptions): { model: Model; level: Level } {
  const level = mapLevel(model, opts.previousLevel)
  let view = model
  let viewLevel = level
  if (opts.focus) {
    view = neighbourhood(model, opts.focus)
    viewLevel = mapLevel(view)
  }
  const problems = opts.problems ?? viewLevel === 'collapsed'
  return { model: fit(shape(view, viewLevel, problems, opts.expanded)), level }
}

// The view of the map in the address bar: /?focus=guest:qemu/101&expand=...
export interface MapView {
  focus?: string
  expanded: Set<string>
  problems?: boolean
}

export function readMapView(search: string): MapView {
  const q = new URLSearchParams(search)
  const view: MapView = { expanded: new Set(q.getAll('expand').filter(Boolean)) }
  const focus = q.get('focus')
  if (focus) view.focus = focus
  const problems = q.get('problems')
  if (problems === '1' || problems === '0') view.problems = problems === '1'
  return view
}

// mapViewQuery writes a view into the query, keeping what else it holds.
export function mapViewQuery(search: string, view: MapView): string {
  const q = new URLSearchParams(search)
  q.delete('focus')
  q.delete('expand')
  q.delete('problems')
  if (view.focus) q.set('focus', view.focus)
  for (const id of [...view.expanded].sort()) q.append('expand', id)
  if (view.problems !== undefined) q.set('problems', view.problems ? '1' : '0')
  const s = q.toString()
  return s ? `?${s}` : ''
}

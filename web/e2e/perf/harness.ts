// The page of the flow map's performance test (spec-ui 12.2). It builds a
// graph at the map's render budget, draws it with the renderer the query
// names (?renderer=baseline, or ?renderer=map for src/flow/FlowMap.tsx),
// moves its dots with one loop, and measures what perf.spec.ts asks for
// through window.perf.

import '../../src/theme/tokens.css'

import { createElement as h, useSyncExternalStore } from 'react'
import { createRoot } from 'react-dom/client'

import type { Box, FlowEdge, FlowMap, FlowNode, FlowRow, Layout, Model, Motion, MotionEdge, Point } from '../../src/flow/types'

// The render budget of spec-ui 5.4: 150 cards (46 zones, 12 tunnels with a
// connector each, 20 paths, 60 targets) and 400 edges; the dot cap of 5.3.
const zones = 46
const tunnels = 12
const paths = 20
const targets = 60
const cards = 150
const lines = 400
const cap = 400
const changed = 20 // routes one state change changes
const crossing = 1.6 // seconds a dot takes along its edge

// The bands as task 11b lays them out, in layout units: [x, width].
const bands = {
  hostnames: [8, 300],
  edge: [350, 172],
  connector: [662, 168],
  path: [872, 134],
  targets: [1036, 200],
} as const
const width = 1244
const top = 34
const row = 24
const header = 36
const port = 22
const targetHeader = 30
const tunnelHeight = 96
const pathHeight = 52

// The trunk of the first tunnel and every port edge whose route number is
// below 5 modulo 12 carry dots: the trunk and 100 port edges. Through
// dotsPerSecond their rates give 1.6 to 14 dots a second.
const movingPorts = 100
const trunkRate = 1000
const rates = [0.05, 0.5, 1, 2, 4, 7, 12, 20, 40, 80, 150, 300, 700, 2000]
const moves = (route: number) => route % 12 < 5

const states = ['active', 'unreachable', 'withdrawn']
const initial = (route: number) => (route % 7 === 3 ? 'unreachable' : route % 11 === 5 ? 'withdrawn' : 'active')
const tagsOf = (route: number, state: string) => [...(state === 'withdrawn' ? ['503'] : []), ...(route % 13 === 0 ? ['DNS'] : [])]
const lineOf = (state: string): FlowEdge['style'] => (state === 'unreachable' || state === 'withdrawn' ? state : 'plain')

function must<T>(value: T | undefined, what: string): T {
  if (value === undefined) throw new Error(`the graph has no ${what}`)
  return value
}

// Where a route shows: its row in a zone card, its access point on a target
// card and its port edge, as indexes into the model.
interface Where {
  zone: number
  row: number
  target: number
  port: number
  edge: number
}

export interface Graph {
  model: Model
  layout: Layout
  routes: Where[]
}

// graph is the synthetic map: 240 routes, each a row in a zone card, an
// access point on a target card and the port edge to it.
export function graph(): Graph {
  const boxes = new Map<string, Box>()
  const ends = new Map<string, { from: Point; to: Point }>()
  const box = (id: string, b: Box) => {
    boxes.set(id, b)
    return b
  }
  const right = (b: Box): Point => ({ x: b.x + b.width, y: b.y + b.height / 2 })
  const left = (b: Box): Point => ({ x: b.x, y: b.y + b.height / 2 })

  const targetNodes: FlowNode[] = []
  const access: Point[] = []
  const owner: string[] = []
  const placeOf: { target: number; port: number }[] = []
  let y = top
  for (let j = 0; j < targets; j++) {
    const count = 2 + (j % 5)
    const first = access.length
    const b = box(`target/${j}`, { x: bands.targets[0], y, width: bands.targets[1], height: targetHeader + count * port + 6 })
    const ports = Array.from({ length: count }, (_, q) => ({ id: `target/${j}:${8080 + q}`, label: `:${8080 + q} http`, state: initial(first + q) }))
    targetNodes.push({ id: `target/${j}`, band: 'targets', kind: 'target', label: `guest-${j}`, state: 'running', ports })
    for (let q = 0; q < count; q++) {
      access.push({ x: b.x, y: y + targetHeader + q * port + (port - 4) / 2 })
      owner.push(`qemu/${100 + j}`)
      placeOf.push({ target: j, port: q })
    }
    y += b.height + 10
  }
  let height = y

  const zoneNodes: FlowNode[] = []
  const rowOf: { zone: number; row: number }[] = []
  y = top
  for (let i = 0; i < zones; i++) {
    const count = 3 + (i % 5) + (i < 12 ? 1 : 0)
    const first = rowOf.length
    const rows: FlowRow[] = Array.from({ length: count }, (_, r) => {
      const route = first + r
      const state = initial(route)
      return { hostname: `app-${route}.zone-${i}.example`, owner: must(owner[route], `owner of route ${route}`), state, tags: tagsOf(route, state) }
    })
    const b = box(`zone/${i}`, { x: bands.hostnames[0], y, width: bands.hostnames[1], height: header + count * row + 8 })
    zoneNodes.push({ id: `zone/${i}`, band: 'hostnames', kind: 'zone', label: `zone-${i}.example`, state: 'active', rows })
    for (let r = 0; r < count; r++) rowOf.push({ zone: i, row: r })
    y += b.height + 12
  }
  height = Math.max(height, y) + 10

  const edgeNodes: FlowNode[] = []
  const connectorNodes: FlowNode[] = []
  for (let t = 0; t < tunnels; t++) {
    const ty = top + ((t + 0.5) * (height - top)) / tunnels - tunnelHeight / 2
    box(`edge/${t}`, { x: bands.edge[0], y: ty, width: bands.edge[1], height: tunnelHeight })
    box(`connector/${t}`, { x: bands.connector[0], y: ty, width: bands.connector[1], height: tunnelHeight })
    const tunnel = `pco-${(0x3f9a1c2b + t * 7919).toString(16)}`
    edgeNodes.push({ id: `edge/${t}`, band: 'edge', kind: 'edge', label: tunnel, state: '4 connections' })
    connectorNodes.push({ id: `connector/${t}`, band: 'connector', kind: 'connector', label: `pve${t + 1}`, state: 'ready' })
  }
  const pathNodes: FlowNode[] = []
  for (let p = 0; p < paths; p++) {
    const py = top + ((p + 0.5) * (height - top)) / paths - pathHeight / 2
    box(`path/${p}`, { x: bands.path[0], y: py, width: bands.path[1], height: pathHeight })
    pathNodes.push({ id: `path/${p}`, band: 'path', kind: 'path', label: `vmbr${p % 4} · VLAN ${10 + p}`, state: 'direct' })
  }
  const at = (id: string) => must(boxes.get(id), id)

  const edges: FlowEdge[] = []
  const link = (edge: FlowEdge, from: Point, to: Point) => {
    edges.push(edge)
    ends.set(edge.id, { from, to })
  }
  for (let i = 0; i < zones; i++) {
    const t = Math.floor((i * tunnels) / zones)
    link({ id: `hairline/${i}`, from: `zone/${i}`, to: `edge/${t}`, style: 'hairline', label: `${zoneNodes[i]?.rows?.length ?? 0} rules` }, right(at(`zone/${i}`)), left(at(`edge/${t}`)))
    if (i < 2) {
      const other = tunnels - 1 - i
      link({ id: `hairline/${i}/${other}`, from: `zone/${i}`, to: `edge/${other}`, style: 'hairline' }, right(at(`zone/${i}`)), left(at(`edge/${other}`)))
    }
  }
  for (let t = 0; t < tunnels; t++) {
    const busy = t === 0 ? { lanes: 4, rate: trunkRate } : { lanes: 2 }
    link({ id: `trunk/${t}`, from: `edge/${t}`, to: `connector/${t}`, style: 'trunk', ...busy }, right(at(`edge/${t}`)), left(at(`connector/${t}`)))
  }
  for (let p = 0; p < paths; p++) {
    for (let k = 0; k < 5; k++) {
      const c = (p + k) % tunnels
      link({ id: `feed/${c}/${p}`, from: `connector/${c}`, to: `path/${p}`, style: 'plain' }, right(at(`connector/${c}`)), left(at(`path/${p}`)))
    }
  }
  const routes: Where[] = []
  const before = zones + 2 * tunnels + paths
  let moving = 0
  for (let route = 0; route < access.length; route++) {
    const { target, port: q } = must(placeOf[route], `target of route ${route}`)
    const { zone, row: r } = must(rowOf[route], `row of route ${route}`)
    const p = Math.floor(target / 3)
    const rate = moves(route) ? rates[moving++ % rates.length] : undefined
    routes.push({ zone, row: r, target: before + target, port: q, edge: edges.length })
    link(
      { id: `port/${route}`, from: `path/${p}`, to: `target/${target}`, style: lineOf(initial(route)), ...(rate === undefined ? {} : { rate }) },
      right(at(`path/${p}`)),
      must(access[route], `access point of route ${route}`),
    )
  }

  const nodes = [...zoneNodes, ...edgeNodes, ...connectorNodes, ...pathNodes, ...targetNodes]
  if (nodes.length !== cards || edges.length !== lines || moving !== movingPorts || rowOf.length !== access.length) {
    throw new Error(`the graph has ${nodes.length} cards, ${edges.length} edges, ${moving} moving and ${rowOf.length} rows for ${access.length} ports`)
  }
  return { model: { nodes, edges }, layout: { width, height, nodes: boxes, edges: ends }, routes }
}

// change is a state notice that moves 20 routes on to their next state: the
// rows, the access points and the port edges of those routes are new
// objects, everything else is kept.
export function change(model: Model, routes: readonly Where[], step: number): Model {
  const nodes = model.nodes.slice()
  const edges = model.edges.slice()
  for (let j = 0; j < changed; j++) {
    const route = ((step * changed + j) * 7919) % routes.length
    const where = must(routes[route], `route ${route}`)
    const zone = must(nodes[where.zone], `zone of route ${route}`)
    const target = must(nodes[where.target], `target of route ${route}`)
    const edge = must(edges[where.edge], `edge of route ${route}`)
    const rows = must(zone.rows, `rows of ${zone.id}`)
    const ports = must(target.ports, `ports of ${target.id}`)
    const was = must(rows[where.row], `row of route ${route}`)
    const state = must(states[(states.indexOf(was.state) + 1) % states.length], 'state')
    nodes[where.zone] = { ...zone, rows: rows.with(where.row, { ...was, state, tags: tagsOf(route, state) }) }
    nodes[where.target] = { ...target, ports: ports.with(where.port, { ...must(ports[where.port], `port of route ${route}`), state }) }
    edges[where.edge] = { ...edge, style: lineOf(state) }
  }
  return { nodes, edges }
}

export function dotsPerSecond(rate: number): number {
  return rate <= 0 ? 0 : Math.min(14, Math.max(1.5, 1.5 + 4 * Math.log10(1 + rate)))
}

interface Dot {
  edge: MotionEdge
  t: number
  offset: number // across the edge, for the lanes of the trunk
  el: SVGCircleElement
}

// createMotion is the one animation loop every renderer of the page shares:
// at most cap dots, the trunk served first and then the port edges by rate,
// each dot crossing its edge in 1.6 s.
export function createMotion(cap: number): Motion & { dots(): number; layer(): SVGGElement | null } {
  let layer: SVGGElement | null = null
  let edges: MotionEdge[] = []
  let visible: ReadonlySet<string> | undefined
  let paused = false
  let last: number | undefined
  let lane = 0
  const owed = new Map<string, number>()
  let live: Dot[] = []
  let spare: SVGCircleElement[] = []

  const rank = (e: MotionEdge) => (e.edge.style === 'trunk' ? Infinity : (e.edge.rate ?? 0))
  const retire = (dot: Dot) => {
    dot.el.setAttribute('visibility', 'hidden')
    spare.push(dot.el)
  }
  const spawn = (edge: MotionEdge, target: SVGGElement) => {
    let el = spare.pop()
    if (!el) {
      el = document.createElementNS('http://www.w3.org/2000/svg', 'circle')
      el.setAttribute('r', '3')
      target.append(el)
    }
    el.removeAttribute('visibility')
    const lanes = edge.edge.lanes ?? 1
    const offset = lanes > 1 ? ((lane++ % lanes) - (lanes - 1) / 2) * 3 : 0
    live.push({ edge, t: 0, offset, el })
  }
  const place = (dot: Dot) => {
    const p = dot.edge.at(dot.t)
    let { x, y } = p
    if (dot.offset !== 0) {
      const a = dot.edge.at(Math.max(0, dot.t - 0.01))
      const b = dot.edge.at(Math.min(1, dot.t + 0.01))
      const length = Math.hypot(b.x - a.x, b.y - a.y) || 1
      x += (-(b.y - a.y) / length) * dot.offset
      y += ((b.x - a.x) / length) * dot.offset
    }
    dot.el.setAttribute('transform', `translate(${x.toFixed(1)} ${y.toFixed(1)})`)
  }

  const frame = (now: number) => {
    const dt = last === undefined ? 0 : Math.min(0.1, (now - last) / 1000)
    last = now
    if (layer && !paused) {
      for (const edge of edges) {
        if (visible && !visible.has(edge.edge.id)) continue
        let due = (owed.get(edge.edge.id) ?? 0) + dt * dotsPerSecond(edge.edge.rate ?? 0)
        for (; due >= 1; due--) if (live.length < cap) spawn(edge, layer)
        owed.set(edge.edge.id, due)
      }
      live = live.filter((dot) => {
        dot.t += dt / crossing
        if (dot.t >= 1) {
          retire(dot)
          return false
        }
        place(dot)
        return true
      })
    }
    requestAnimationFrame(frame)
  }
  requestAnimationFrame(frame)

  return {
    attach(next) {
      if (next === layer) return
      layer = next
      live = []
      spare = []
    },
    setEdges(next) {
      edges = [...next].sort((a, b) => rank(b) - rank(a))
      const byId = new Map(edges.map((e) => [e.edge.id, e]))
      live = live.filter((dot) => {
        const edge = byId.get(dot.edge.edge.id)
        if (edge) dot.edge = edge
        else retire(dot)
        return edge !== undefined
      })
    },
    setVisible(ids) {
      visible = ids
      live = live.filter((dot) => {
        if (!ids.has(dot.edge.edge.id)) retire(dot)
        return ids.has(dot.edge.edge.id)
      })
    },
    pause() {
      paused = true
    },
    resume() {
      paused = false
    },
    dots: () => live.length,
    layer: () => layer,
  }
}

function store<T>(value: T) {
  const listeners = new Set<() => void>()
  return {
    get: () => value,
    set(next: T) {
      value = next
      for (const listener of listeners) listener()
    },
    subscribe(listener: () => void) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}

// A frame as the page saw it: the time its animation callbacks began, the
// time its rendering was done (the first task after it), and the frame's
// timestamp.
interface Frame {
  began: number
  painted: number
  stamp: number
}

const channel = new MessageChannel()
const waiting: (() => void)[] = []
channel.port1.onmessage = () => waiting.shift()?.()

function nextFrame(): Promise<Frame> {
  return new Promise((resolve) =>
    requestAnimationFrame((stamp) => {
      const began = performance.now()
      waiting.push(() => resolve({ began, painted: performance.now(), stamp }))
      channel.port2.postMessage(null)
    }),
  )
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

export interface FrameRun {
  intervals: number[] // between the timestamps of consecutive frames, ms
  work: number[] // from a frame's animation callbacks to its rendering done, ms
  longTasks: number[] // durations of the tasks over 50 ms, ms
  dots: number // dots on the map, the mean over the frames
}

export interface Perf {
  ready: boolean
  error?: string
  stateToPaint(count: number, warmup: number): Promise<number[]>
  frames(ms: number): Promise<FrameRun>
}

declare global {
  interface Window {
    perf?: Perf
  }
}

const maps = import.meta.glob<{ default: FlowMap }>('../../src/flow/FlowMap.tsx')
const renderers: Record<string, (() => Promise<{ default: FlowMap }>) | undefined> = {
  baseline: () => import('./baseline'),
  map: Object.values(maps)[0],
}

async function start(perf: Perf): Promise<void> {
  const name = new URLSearchParams(location.search).get('renderer') ?? 'baseline'
  const load = renderers[name]
  if (!load) throw new Error(`no renderer ${name} on this page`)
  const { default: Renderer } = await load()

  const g = graph()
  const model = store(g.model)
  const motion = createMotion(cap)
  const container = must(document.getElementById('map') ?? undefined, 'element #map')
  // The map fits the width, as it does on the Overview; it is given the
  // height of the whole graph, so every card, edge and dot is in its view.
  const scale = innerWidth / g.layout.width
  container.style.width = `${innerWidth}px`
  container.style.height = `${Math.ceil(g.layout.height * scale)}px`

  const ignore = () => undefined
  function Root() {
    const current = useSyncExternalStore(model.subscribe, model.get)
    return h(Renderer, {
      model: current,
      layout: g.layout,
      motion,
      onSelect: ignore,
      onFocus: ignore,
      onExpand: ignore,
      onViewport: ignore,
      reducedMotion: false,
    })
  }
  createRoot(container).render(h(Root))

  // A change of the map is a mutation of its elements that is not a dot.
  let changedAt = -Infinity
  new MutationObserver((records) => {
    const dots = motion.layer()
    if (records.some((r) => !dots?.contains(r.target))) changedAt = performance.now()
  }).observe(container, { subtree: true, childList: true, attributes: true, characterData: true })

  // stateToPaint applies one change and returns the time until the first
  // frame painted after the map's last change for it: the commit, and
  // whatever the renderer does in the frames after. The change waits a
  // different fraction of a frame each time, as a notice from the network
  // would.
  let step = 0
  async function once(): Promise<number> {
    await nextFrame()
    await sleep(step % 17)
    changedAt = -Infinity
    const start = performance.now()
    model.set(change(model.get(), g.routes, step++))
    const frames: Frame[] = []
    for (let quiet = 0; quiet < 3; ) {
      const f = await nextFrame()
      frames.push(f)
      quiet = changedAt < f.began ? quiet + 1 : 0
      if (frames.length > 300) throw new Error('the map did not settle within 300 frames')
    }
    const painted = frames.find((f) => f.painted > changedAt)
    if (changedAt < start || !painted) throw new Error('the map did not change')
    return painted.painted - start
  }

  // The renderer is ready when it has attached the dots' layer and
  // registered its moving edges.
  const since = performance.now()
  while (motion.dots() === 0) {
    if (performance.now() - since > 10_000) throw new Error(`${name} drew no dots: no layer attached or no moving edge registered`)
    await nextFrame()
  }

  perf.stateToPaint = async (count, warmup) => {
    const out: number[] = []
    for (let i = 0; i < warmup + count; i++) {
      const ms = await once()
      if (i >= warmup) out.push(ms)
    }
    return out
  }
  perf.frames = async (ms) => {
    const longTasks: number[] = []
    const watch = new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) longTasks.push(entry.duration)
    })
    watch.observe({ type: 'longtask' })
    const intervals: number[] = []
    const work: number[] = []
    let dots = 0
    let previous: number | undefined
    const end = performance.now() + ms
    while (performance.now() < end) {
      const f = await nextFrame()
      if (previous !== undefined) intervals.push(f.stamp - previous)
      previous = f.stamp
      work.push(f.painted - f.began)
      dots += motion.dots()
    }
    watch.disconnect()
    return { intervals, work, longTasks, dots: dots / work.length }
  }
  perf.ready = true
}

const perf: Perf = {
  ready: false,
  stateToPaint: () => Promise.reject(new Error('the page is not ready')),
  frames: () => Promise.reject(new Error('the page is not ready')),
}
window.perf = perf
start(perf).catch((err: unknown) => {
  perf.error = err instanceof Error ? err.message : String(err)
})

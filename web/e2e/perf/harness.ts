// The page of the flow map's performance test. It builds the scene the
// query names (?scene=budget, the default, a graph at the map's render
// budget, or ?scene=large, outage or wide, the states of the fake daemon's
// scenarios through the map's own model, folding and layout), draws it with
// the renderer the query names (?renderer=baseline, or ?renderer=map for
// src/flow/FlowMap.tsx), moves its dots with the map's own loop, and
// measures what perf.spec.ts asks for through window.perf.

import '../../src/theme/tokens.css'

import { createElement as h, useSyncExternalStore } from 'react'
import { createRoot } from 'react-dom/client'

import type { State, TrafficView } from '../../src/api/types.gen'
import { collapse, type Level } from '../../src/flow/collapse'
import { layout as layOut } from '../../src/flow/layout'
import { buildModel } from '../../src/flow/model'
import { createMotion, dotCap } from '../../src/flow/motion'
import { keepSame } from '../../src/flow/panel'
import { large, outage, trafficFor, wide } from '../../src/flow/testdata/scale'
import type { Box, FlowEdge, FlowMap, FlowNode, FlowRow, Layout, Model, Motion, Point } from '../../src/flow/types'

// The render budget: 150 cards (46 zones, 12 tunnels with a connector each,
// 20 paths, 60 targets) and 400 edges; at most 400 dots at once.
const zones = 46
const tunnels = 12
const paths = 20
const targets = 60
const cards = 150
const lines = 400
const changed = 20 // routes one state change changes

// The bands of the layout, in layout units: [x, width].
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
// below 5 modulo 12 carry dots: the trunk and 100 port edges. Through the
// loop's dotsPerSecond their rates give 1.6 to 14 dots a second.
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

// A View is what a renderer is given: a model and where layout put it.
interface View {
  model: Model
  layout: Layout
}

// A Scene is a first view and the views after the state notices that follow
// it, each of which changes 20 routes.
interface Scene {
  first: View
  next(): View
  // What the model, the folding and the layout took for each notice, ms.
  pipeline: number[]
}

function budgetScene(): Scene {
  const g = graph()
  let model = g.model
  let step = 0
  return {
    first: { model, layout: g.layout },
    next() {
      model = change(model, g.routes, step++)
      return { model, layout: g.layout }
    },
    pipeline: [],
  }
}

const scenarios: Record<string, (() => State) | undefined> = { large, outage, wide }

// The states a notice moves a route through.
const cycle = ['active', 'unreachable', 'withdrawn']

// stateScene is a scenario of the fake daemon as the Overview draws it: the
// model of its state, folded to the budget with the level held from the
// view before, and laid out from the view before. A notice moves 20 of its
// routes with a target on to their next state.
function stateScene(name: string): Scene {
  const make = must(scenarios[name], `scene ${name}`)
  let st = make()
  const traffic: TrafficView = trafficFor(st)
  const served = st.routes.flatMap((r, i) => (r.service ? [{ i, service: r.service }] : []))
  let level: Level | undefined
  let view: View | undefined
  const draw = (): View => {
    const shaped = collapse(buildModel(st, traffic), { expanded: new Set(), previousLevel: level })
    level = shaped.level
    const model = keepSame(view?.model, shaped.model)
    view = { model, layout: layOut(model, view?.layout) }
    return view
  }
  const first = draw()
  const pipeline: number[] = []
  let step = 0
  return {
    first,
    next() {
      const routes = st.routes.slice()
      for (let j = 0; j < changed; j++) {
        const { i, service } = must(served[((step * changed + j) * 7919) % served.length], 'a route with a target')
        const r = must(routes[i], `route ${i}`)
        const state = must(cycle[(cycle.indexOf(r.state) + 1) % cycle.length], 'state')
        const answers = state !== 'withdrawn'
        const reason = state === 'unreachable' ? 'target is not answering' : state === 'withdrawn' ? 'identity check failed' : undefined
        routes[i] = { ...r, state, reason, service: answers ? service : undefined, rule: r.rule && { ...r.rule, service: answers ? service : 'http_status:503' } }
      }
      step++
      st = { ...st, routes }
      const t0 = performance.now()
      const v = draw()
      pipeline.push(performance.now() - t0)
      return v
    },
    pipeline,
  }
}

// The notices a run may take, made before it starts, so that a run measures
// the drawing and not the making.
const planned = 60

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
  // What the model, the folding and the layout took for each notice of a
  // scenario of the fake daemon, ms; none for the budget's graph.
  pipeline: number[]
  cards: number
  edges: number
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
  const query = new URLSearchParams(location.search)
  const name = query.get('renderer') ?? 'baseline'
  const load = renderers[name]
  if (!load) throw new Error(`no renderer ${name} on this page`)
  const { default: Renderer } = await load()

  const sceneName = query.get('scene') ?? 'budget'
  const scene = sceneName === 'budget' ? budgetScene() : stateScene(sceneName)
  const views = [scene.first]
  for (let i = 0; i < planned; i++) views.push(scene.next())
  perf.pipeline = scene.pipeline
  perf.cards = scene.first.model.nodes.length
  perf.edges = scene.first.model.edges.length
  const shown = store<View>(scene.first)

  // The map's own loop; the page keeps the group it draws into, to tell its
  // dots from the map's changes.
  const loop = createMotion({ cap: dotCap })
  let layer: SVGGElement | null = null
  const motion: Motion = {
    ...loop,
    attach(g) {
      layer = g
      loop.attach(g)
    },
  }

  const container = must(document.getElementById('map') ?? undefined, 'element #map')
  // The map fits the width, as it does on the Overview; it is given the
  // height of the tallest view, so every card, edge and dot is in its view.
  const scale = innerWidth / scene.first.layout.width
  container.style.width = `${innerWidth}px`
  container.style.height = `${Math.ceil(Math.max(...views.map((v) => v.layout.height)) * scale)}px`

  const ignore = () => undefined
  function Root() {
    const current = useSyncExternalStore(shown.subscribe, shown.get)
    return h(Renderer, {
      model: current.model,
      layout: current.layout,
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
    if (records.some((r) => !layer?.contains(r.target))) changedAt = performance.now()
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
    shown.set(must(views[1 + step++], `notice ${step}: no more than ${planned} are made`))
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
  while (loop.dots() === 0) {
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
      dots += loop.dots()
    }
    watch.disconnect()
    return { intervals, work, longTasks, dots: dots / work.length }
  }
  perf.ready = true
}

const perf: Perf = {
  ready: false,
  pipeline: [],
  cards: 0,
  edges: 0,
  stateToPaint: () => Promise.reject(new Error('the page is not ready')),
  frames: () => Promise.reject(new Error('the page is not ready')),
}
window.perf = perf
start(perf).catch((err: unknown) => {
  perf.error = err instanceof Error ? err.message : String(err)
})

// The dots of the map: one animation loop moves every dot, each edge gets
// dots by its rate, and nothing moves that does not carry a real figure.
// The trunk carries the tunnel's requests per second, with a share of red
// diamonds for its proxy errors; an edge past the connector carries the
// connections the connector opened to the targets behind it.

import type { FlowEdge, Motion, MotionEdge } from './types'

// A dot crosses its edge in 1.6 s, whatever the rate: its speed means
// nothing, as no latency is known.
export const crossing = 1.6

// At most this many dots are on the map at once.
export const dotCap = 400

// dotsPerSecond maps a rate to dots on a log scale, so that 1 req/s and
// 1000 req/s both read; the exact number is always printed beside them.
export function dotsPerSecond(r: number): number {
  if (!(r > 0)) return 0
  if (!Number.isFinite(r)) return 14
  return Math.min(14, Math.max(1.5, 1.5 + 4 * Math.log10(1 + r)))
}

// errorShare is the part of the trunk's dots drawn as red diamonds: the
// errors' part of the requests, at least one in twenty when there was any,
// so that a rare error stays visible.
export function errorShare(e: Pick<FlowEdge, 'rate' | 'errors'>): number {
  const errors = e.errors ?? 0
  if (!(errors > 0)) return 0
  const rps = e.rate ?? 0
  return rps > 0 ? Math.min(1, Math.max(0.05, errors / rps)) : 1
}

// Hostname edges and the edges of rogue connectors never move; an edge
// whose figure is stale has none to show.
const moves = (e: FlowEdge) => e.style !== 'hairline' && e.style !== 'rogue' && !e.stale && dotsPerSecond(e.rate ?? 0) > 0

const svg = 'http://www.w3.org/2000/svg'
const radius = 3
const diamond = 5.5

interface Dot {
  edge: MotionEdge
  t: number
  offset: number // across the edge, for the lanes of the trunk
  error: boolean
  el: SVGElement
}

export interface MotionOptions {
  cap: number
  // The browser's by default; a test gives its own clock.
  frame?: (callback: (now: number) => void) => number
  cancel?: (id: number) => void
  hidden?: () => boolean
  reducedMotion?: () => boolean
}

// MotionLoop is the loop with what a test or the page reads of it.
export interface MotionLoop extends Motion {
  dots(): number
  dotsOn(edge: string): number
  errorsOn(edge: string): number
  // stop ends the loop for good, when the map goes.
  stop(): void
}

const reducedQuery = '(prefers-reduced-motion: reduce)'

function browserReduced(): boolean {
  return typeof window !== 'undefined' && typeof window.matchMedia === 'function' && window.matchMedia(reducedQuery).matches
}

// createMotion is the one requestAnimationFrame loop of the map. It moves
// no dot while paused, while the user asks for reduced motion, while the
// page is hidden, and none on an edge outside the viewport. Of the cap, the
// trunk is served first, then the other edges by rate.
export function createMotion(opts: MotionOptions): MotionLoop {
  const cap = Math.max(0, opts.cap)
  const frame = opts.frame ?? ((cb: (now: number) => void) => window.requestAnimationFrame(cb))
  const cancel = opts.cancel ?? ((id: number) => window.cancelAnimationFrame(id))
  const hidden = opts.hidden ?? (() => typeof document !== 'undefined' && document.hidden)
  const reduced = opts.reducedMotion ?? browserReduced

  let layer: SVGGElement | null = null
  let edges: MotionEdge[] = []
  let visible: ReadonlySet<string> | undefined
  let paused = false
  let stopped = false
  let pending: number | undefined
  let last: number | undefined
  let lane = 0
  let live: Dot[] = []
  const owed = new Map<string, number>()
  const errorsOwed = new Map<string, number>()
  const allowance = new Map<string, number>()
  const count = new Map<string, number>()
  const spare: Record<'dot' | 'error', SVGElement[]> = { dot: [], error: [] }

  const rank = (e: MotionEdge) => (e.edge.style === 'trunk' ? Infinity : (e.edge.rate ?? 0))
  const eligible = (e: MotionEdge) => moves(e.edge) && (visible === undefined || visible.has(e.edge.id))
  const running = () => layer !== null && !paused && !stopped && !hidden() && !reduced()

  // plan shares the cap out: each moving edge in turn, the trunk first, is
  // given room for the dots it shows at once.
  const plan = () => {
    allowance.clear()
    let room = cap
    for (const e of edges) {
      if (room <= 0) break
      if (!eligible(e)) continue
      const give = Math.min(Math.ceil(dotsPerSecond(e.edge.rate ?? 0) * crossing) + 1, room)
      allowance.set(e.edge.id, give)
      room -= give
    }
  }

  const retire = (dot: Dot) => {
    dot.el.setAttribute('visibility', 'hidden')
    spare[dot.error ? 'error' : 'dot'].push(dot.el)
    count.set(dot.edge.edge.id, (count.get(dot.edge.edge.id) ?? 1) - 1)
  }

  const clear = () => {
    for (const dot of live) retire(dot)
    live = []
    owed.clear()
    last = undefined
  }

  const spawn = (edge: MotionEdge, target: SVGGElement) => {
    const id = edge.edge.id
    const share = errorShare(edge.edge)
    let error = false
    if (share > 0) {
      const due = (errorsOwed.get(id) ?? 0) + share
      error = due >= 1
      errorsOwed.set(id, error ? due - 1 : due)
    }
    let el = spare[error ? 'error' : 'dot'].pop()
    if (!el) {
      el = document.createElementNS(svg, error ? 'rect' : 'circle')
      if (error) {
        el.setAttribute('width', String(diamond))
        el.setAttribute('height', String(diamond))
        el.setAttribute('class', 'dot dot-error')
      } else {
        el.setAttribute('r', String(radius))
        el.setAttribute('class', 'dot')
      }
      target.append(el)
    }
    el.removeAttribute('visibility')
    const lanes = Math.max(1, Math.min(4, edge.edge.lanes ?? 1))
    const offset = lanes > 1 ? ((lane++ % lanes) - (lanes - 1) / 2) * 3 : 0
    live.push({ edge, t: 0, offset, error, el })
    count.set(id, (count.get(id) ?? 0) + 1)
  }

  const place = (dot: Dot) => {
    let { x, y } = dot.edge.at(dot.t)
    if (dot.offset !== 0) {
      const a = dot.edge.at(Math.max(0, dot.t - 0.01))
      const b = dot.edge.at(Math.min(1, dot.t + 0.01))
      const length = Math.hypot(b.x - a.x, b.y - a.y) || 1
      x += (-(b.y - a.y) / length) * dot.offset
      y += ((b.x - a.x) / length) * dot.offset
    }
    const at = `translate(${x.toFixed(1)} ${y.toFixed(1)})`
    dot.el.setAttribute('transform', dot.error ? `${at} rotate(45) translate(${-diamond / 2} ${-diamond / 2})` : at)
  }

  const tick = (now: number) => {
    pending = undefined
    if (!running() || !layer) {
      clear()
      return
    }
    const dt = last === undefined ? 0 : Math.min(0.1, Math.max(0, now - last) / 1000)
    last = now
    for (const edge of edges) {
      const id = edge.edge.id
      const room = allowance.get(id) ?? 0
      if (room <= 0) continue
      let due = (owed.get(id) ?? 0) + dt * dotsPerSecond(edge.edge.rate ?? 0)
      for (; due >= 1; due--) {
        if ((count.get(id) ?? 0) >= room || live.length >= cap) {
          due = Math.min(due, 1)
          break
        }
        spawn(edge, layer)
      }
      owed.set(id, due)
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
    if (live.length > 0 || allowance.size > 0) pending = frame(tick)
  }

  const wake = () => {
    if (pending === undefined && running() && (live.length > 0 || allowance.size > 0)) pending = frame(tick)
  }

  const halt = () => {
    if (pending !== undefined) cancel(pending)
    pending = undefined
    clear()
  }

  const changed = () => (running() ? wake() : halt())
  const doc = typeof document !== 'undefined' ? document : undefined
  doc?.addEventListener('visibilitychange', changed)
  const query = typeof window !== 'undefined' && typeof window.matchMedia === 'function' ? window.matchMedia(reducedQuery) : undefined
  query?.addEventListener('change', changed)

  // keepLive drops the dots of edges that no longer move or are gone, and
  // moves the others onto their edge as now drawn.
  const keepLive = () => {
    const byId = new Map(edges.map((e) => [e.edge.id, e]))
    live = live.filter((dot) => {
      const edge = byId.get(dot.edge.edge.id)
      if (!edge || !allowance.has(edge.edge.id)) {
        retire(dot)
        return false
      }
      dot.edge = edge
      return true
    })
  }

  return {
    attach(next) {
      if (next === layer) return
      halt()
      spare.dot = []
      spare.error = []
      count.clear()
      layer = next
      wake()
    },
    setEdges(next) {
      edges = [...next].sort((a, b) => rank(b) - rank(a) || (a.edge.id < b.edge.id ? -1 : a.edge.id > b.edge.id ? 1 : 0))
      plan()
      keepLive()
      wake()
    },
    setVisible(ids) {
      visible = ids
      plan()
      keepLive()
      wake()
    },
    pause() {
      paused = true
      halt()
    },
    resume() {
      paused = false
      wake()
    },
    dots: () => live.length,
    dotsOn: (id) => live.filter((d) => d.edge.edge.id === id).length,
    errorsOn: (id) => live.filter((d) => d.edge.edge.id === id && d.error).length,
    stop() {
      stopped = true
      halt()
      doc?.removeEventListener('visibilitychange', changed)
      query?.removeEventListener('change', changed)
    },
  }
}

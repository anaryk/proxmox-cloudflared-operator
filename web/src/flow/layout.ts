// Where the map draws each card and edge, in layout units: five fixed
// bands, left to right, 1244 units wide; the renderer scales them. The order
// within a band is by the barycentre of each node's neighbours, two sweeps,
// ties by name; a node keeps its place among the others across updates and
// new ones are inserted, so the map does not jump with every cycle.

import { noZone } from './model'
import type { Band, Box, FlowEdge, FlowNode, Layout, Model, Point } from './types'

export const layoutWidth = 1244

export const bands: Readonly<Record<Band, { x: number; width: number }>> = {
  hostnames: { x: 8, width: 300 },
  edge: { x: 350, width: 172 },
  connector: { x: 662, width: 168 },
  path: { x: 872, width: 134 },
  targets: { x: 1036, width: 200 },
}

const bandOrder: readonly Band[] = ['hostnames', 'edge', 'connector', 'path', 'targets']

// The heights of the parts of a card. A zone card is its header, a line of
// counts when collapsed, its rows and a margin; a target card is its header
// and an access point per port.
export const top = 34
export const rowHeight = 24
export const cardHeader = 36
export const cardFoot = 8
export const targetHeader = 30
export const portHeight = 22
export const tunnelHeight = 96
export const smallHeight = 52
const gap = 12
const bottom = 10

export function heightOf(n: FlowNode): number {
  switch (n.kind) {
    case 'zone':
      return cardHeader + ((n.counts ? 1 : 0) + (n.rows?.length ?? 0)) * rowHeight + cardFoot
    case 'edge':
    case 'connector':
      return tunnelHeight
    case 'target':
      return Math.max(smallHeight, targetHeader + (n.ports?.length ?? 0) * portHeight + 6)
  }
  return smallHeight
}

// portY is the middle of the nth access point of a target card at y.
export const portY = (y: number, at: number): number => y + targetHeader + at * portHeight + (portHeight - 4) / 2

const byName = (a: FlowNode, b: FlowNode) => (a.label < b.label ? -1 : a.label > b.label ? 1 : a.id < b.id ? -1 : a.id > b.id ? 1 : 0)

// Zone cards come before the card of hostnames without a zone and before
// folded groups.
const lastRank = (n: FlowNode) => (n.id === noZone || n.kind === 'group' ? 1 : 0)

function initialOrder(nodes: readonly FlowNode[]): FlowNode[] {
  return [...nodes].sort((a, b) => lastRank(a) - lastRank(b) || byName(a, b))
}

// sweep orders a band by the mean position of each node's neighbours in
// the band beside it. A node without one follows a node of its band that
// shares a neighbour with it, as a connector pco does not run follows the
// tunnel's own; else it keeps its own position.
function sweep(band: FlowNode[], beside: readonly FlowNode[], neighbours: ReadonlyMap<string, readonly string[]>): FlowNode[] {
  const there = new Map(beside.map((n, i) => [n.id, (i + 0.5) / beside.length]))
  const keys = new Map<string, number>()
  for (const n of band) {
    const at = (neighbours.get(n.id) ?? []).map((id) => there.get(id)).filter((p): p is number => p !== undefined)
    if (at.length > 0) keys.set(n.id, at.reduce((a, b) => a + b, 0) / at.length)
  }
  const keyed = band.map((n, i) => {
    let key = keys.get(n.id)
    for (const via of key === undefined ? (neighbours.get(n.id) ?? []) : []) {
      const sibling = (neighbours.get(via) ?? []).find((id) => keys.has(id))
      if (sibling !== undefined) {
        key = (keys.get(sibling) ?? 0) + 1e-9
        break
      }
    }
    return { n, key: key ?? (i + 0.5) / band.length }
  })
  keyed.sort((a, b) => lastRank(a.n) - lastRank(b.n) || a.key - b.key || byName(a.n, b.n))
  return keyed.map((k) => k.n)
}

// stable keeps the nodes of before in their order and inserts each new one
// after the node it follows in the fresh order.
function stable(fresh: readonly string[], before: readonly string[] | undefined): string[] {
  if (!before || before.length === 0) return [...fresh]
  const now = new Set(fresh)
  const out = before.filter((id) => now.has(id))
  const placed = new Set(out)
  fresh.forEach((id, i) => {
    if (placed.has(id)) return
    let at = 0
    for (let j = i - 1; j >= 0; j--) {
      const prev = fresh[j]
      if (prev !== undefined && placed.has(prev)) {
        at = out.indexOf(prev) + 1
        break
      }
    }
    out.splice(at, 0, id)
    placed.add(id)
  })
  return out
}

// The order of each band in a layout, read back from where it drew them.
function ordersOf(previous: Layout): Map<Band, string[]> {
  const out = new Map<Band, string[]>()
  for (const band of bandOrder) {
    const x = bands[band].x
    const ids = [...previous.nodes].filter(([, b]) => b.x === x).sort((a, b) => a[1].y - b[1].y)
    out.set(
      band,
      ids.map(([id]) => id),
    )
  }
  return out
}

const centre = (b: Box) => b.y + b.height / 2

export function layout(model: Model, previous?: Layout): Layout {
  const neighbours = new Map<string, string[]>()
  const near = (a: string, b: string) => {
    const list = neighbours.get(a)
    if (list) list.push(b)
    else neighbours.set(a, [b])
  }
  for (const e of model.edges) {
    near(e.from, e.to)
    near(e.to, e.from)
  }

  const inBand = new Map<Band, FlowNode[]>(bandOrder.map((b) => [b, initialOrder(model.nodes.filter((n) => n.band === b))]))
  const get = (b: Band) => inBand.get(b) ?? []
  for (let i = 1; i < bandOrder.length; i++) {
    const b = bandOrder[i] as Band
    inBand.set(b, sweep(get(b), get(bandOrder[i - 1] as Band), neighbours))
  }
  for (let i = bandOrder.length - 2; i >= 0; i--) {
    const b = bandOrder[i] as Band
    inBand.set(b, sweep(get(b), get(bandOrder[i + 1] as Band), neighbours))
  }

  const before = previous ? ordersOf(previous) : undefined
  const nodeOf = new Map(model.nodes.map((n) => [n.id, n]))
  const boxes = new Map<string, Box>()

  const place = (band: Band, wanted: (n: FlowNode) => number | undefined) => {
    const ids = stable(
      get(band).map((n) => n.id),
      before?.get(band),
    )
    let y = top - gap
    for (const id of ids) {
      const n = nodeOf.get(id)
      if (!n) continue
      const height = heightOf(n)
      const want = wanted(n)
      const at = Math.max(y + gap, top, want === undefined ? -Infinity : want - height / 2)
      boxes.set(id, { x: bands[band].x, y: at, width: bands[band].width, height })
      y = at + height
    }
  }
  // The wanted centre of a node: the mean of its neighbours' already drawn.
  const meanOf = (n: FlowNode, band: Band): number | undefined => {
    const ys = (neighbours.get(n.id) ?? []).filter((id) => nodeOf.get(id)?.band === band).map((id) => boxes.get(id)).filter((b): b is Box => b !== undefined).map(centre)
    return ys.length > 0 ? ys.reduce((a, b) => a + b, 0) / ys.length : undefined
  }

  place('hostnames', () => undefined)
  place('targets', () => undefined)
  place('edge', (n) => meanOf(n, 'hostnames'))
  place('connector', (n) => meanOf(n, 'edge'))
  place('path', (n) => meanOf(n, 'targets') ?? meanOf(n, 'connector'))

  const edges = new Map<string, { from: Point; to: Point }>()
  for (const e of model.edges) {
    const a = boxes.get(e.from)
    const b = boxes.get(e.to)
    if (!a || !b) continue
    edges.set(e.id, { from: { x: a.x + a.width, y: centre(a) }, to: { x: b.x, y: endY(e, b, nodeOf.get(e.to)) } })
  }

  let height = 0
  for (const b of boxes.values()) height = Math.max(height, b.y + b.height)
  return { width: layoutWidth, height: Math.max(height, top) + bottom, nodes: boxes, edges }
}

// An edge ends at its access point, else at the header of a target card,
// else at the middle of its card.
function endY(e: FlowEdge, b: Box, n: FlowNode | undefined): number {
  if (e.port && n?.ports) {
    const at = n.ports.findIndex((p) => p.id === e.port)
    if (at >= 0) return portY(b.y, at)
  }
  if (n?.kind === 'target') return b.y + targetHeader / 2
  return centre(b)
}

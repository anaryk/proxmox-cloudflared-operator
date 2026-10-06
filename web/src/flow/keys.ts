// The keyboard of the flow map. The map is one tab stop: it holds one
// focusable item at a time, a card or a line of a zone card, and the keys
// move that focus. Left and Right follow the edges to the neighbour in that
// column, keeping to the chain of a hostname once one is picked; Up and Down
// go to the item above or below in the same column; Home and End go to the
// first and the last column.

import { cardHeader, rowHeight } from './layout'
import type { Band, Box, FlowEdge, FlowNode, FlowRow, Layout, Model } from './types'

const bandOrder: readonly Band[] = ['hostnames', 'edge', 'connector', 'path', 'targets']

// An Item is what can hold the focus: a card, the head of a zone card, or a
// line of one.
export interface Item {
  id: string
  node: FlowNode
  row?: FlowRow
  band: Band
  box: Box
  routes: readonly string[]
}

export interface Nav {
  items: readonly Item[]
  byId: ReadonlyMap<string, Item>
  bands: ReadonlyMap<Band, readonly Item[]>
  out: ReadonlyMap<string, readonly FlowEdge[]>
  in: ReadonlyMap<string, readonly FlowEdge[]>
}

// rowItemId is the id of a line of a zone card: the row's own, or the
// route it stands for.
export const rowItemId = (r: FlowRow): string => r.id ?? `route:${r.hostname} ${r.owner}`

export const rowRoutes = (r: FlowRow): readonly string[] => r.routes ?? (r.kind === 'more' || r.kind === 'group' ? [] : [`${r.hostname} ${r.owner}`])

// rowTop is where the nth line of a zone card at y begins.
export const rowTop = (n: FlowNode, y: number, at: number): number => y + cardHeader + ((n.counts ? 1 : 0) + at) * rowHeight

export function buildNav(model: Model, layout: Layout): Nav {
  const items: Item[] = []
  for (const node of model.nodes) {
    const box = layout.nodes.get(node.id)
    if (!box) continue
    if (node.kind === 'zone' || (node.kind === 'group' && node.band === 'hostnames')) {
      items.push({ id: node.id, node, band: node.band, box: { ...box, height: cardHeader }, routes: node.routes ?? [] })
      ;(node.rows ?? []).forEach((row, at) => {
        items.push({ id: rowItemId(row), node, row, band: node.band, box: { x: box.x, y: rowTop(node, box.y, at), width: box.width, height: rowHeight }, routes: rowRoutes(row) })
      })
    } else {
      items.push({ id: node.id, node, band: node.band, box, routes: node.routes ?? [] })
    }
  }
  const bands = new Map<Band, Item[]>()
  for (const band of bandOrder) {
    bands.set(
      band,
      items.filter((i) => i.band === band).sort((a, b) => a.box.y - b.box.y || a.box.x - b.box.x),
    )
  }
  const out = new Map<string, FlowEdge[]>()
  const into = new Map<string, FlowEdge[]>()
  for (const e of model.edges) {
    out.set(e.from, [...(out.get(e.from) ?? []), e])
    into.set(e.to, [...(into.get(e.to) ?? []), e])
  }
  return { items, byId: new Map(items.map((i) => [i.id, i])), bands, out, in: into }
}

const centre = (i: Item) => i.box.y + i.box.height / 2

function nearest(list: readonly Item[], y: number): Item | undefined {
  let best: Item | undefined
  for (const i of list) if (!best || Math.abs(centre(i) - y) < Math.abs(centre(best) - y)) best = i
  return best
}

export const carries = (i: Item, chain: string | undefined): boolean => chain !== undefined && i.routes.includes(chain)

// chainOf is the chain an item stands for: a hostname's line, one route.
export function chainOf(i: Item): string | undefined {
  return i.row && i.routes.length === 1 ? i.routes[0] : undefined
}

// The item a node is entered at from beside: the line of the chain on a
// zone card, else its head.
function entry(nav: Nav, node: string, chain: string | undefined): Item | undefined {
  const head = nav.byId.get(node)
  if (!head) return undefined
  if (chain !== undefined && head.node.rows) {
    const row = head.node.rows.map((r) => nav.byId.get(rowItemId(r))).find((i) => i !== undefined && carries(i, chain))
    if (row) return row
  }
  return head
}

function beside(nav: Nav, from: Item, dir: 1 | -1, chain: string | undefined): Item | undefined {
  let edges = (dir === 1 ? nav.out : nav.in).get(from.node.id) ?? []
  if (from.row) {
    const mine = new Set(from.routes)
    edges = edges.filter((e) => (e.routes ?? []).some((k) => mine.has(k)))
  }
  const ends = edges.map((e) => entry(nav, dir === 1 ? e.to : e.from, chain)).filter((i): i is Item => i !== undefined)
  const onChain = ends.filter((i) => carries(i, chain))
  const found = nearest(onChain.length > 0 ? onChain : ends, centre(from))
  if (found) return found
  // No edge goes there: the nearest item of the next column that has any,
  // so that every item stays within reach.
  for (let b = bandOrder.indexOf(from.band) + dir; b >= 0 && b < bandOrder.length; b += dir) {
    const list = nav.bands.get(bandOrder[b] as Band) ?? []
    if (list.length > 0) return nearest(list, centre(from))
  }
  return undefined
}

function edgeBand(nav: Nav, last: boolean, from: Item, chain: string | undefined): Item | undefined {
  const order = last ? [...bandOrder].reverse() : bandOrder
  for (const band of order) {
    const list = nav.bands.get(band) ?? []
    if (list.length === 0) continue
    const on = list.filter((i) => carries(i, chain))
    return on.find((i) => i.row) ?? on[0] ?? nearest(list, centre(from))
  }
  return undefined
}

export interface Moved {
  id: string
  chain?: string
}

// step is where a key moves the focus from the item from, along chain when
// one is followed; nothing when the key is not one of the map's.
export function step(nav: Nav, from: string, key: string, chain?: string): Moved | undefined {
  const at = nav.byId.get(from)
  if (!at) {
    const first = nav.items[0]
    return first && { id: first.id, chain: chainOf(first) }
  }
  const followed = chain ?? chainOf(at)
  let to: Item | undefined
  switch (key) {
    case 'ArrowRight':
      to = beside(nav, at, 1, followed)
      break
    case 'ArrowLeft':
      to = beside(nav, at, -1, followed)
      break
    case 'ArrowDown':
    case 'ArrowUp': {
      const list = nav.bands.get(at.band) ?? []
      const i = list.indexOf(at) + (key === 'ArrowDown' ? 1 : -1)
      to = list[Math.min(Math.max(i, 0), list.length - 1)]
      if (to) return { id: to.id, chain: chainOf(to) }
      return undefined
    }
    case 'Home':
      to = edgeBand(nav, false, at, followed)
      break
    case 'End':
      to = edgeBand(nav, true, at, followed)
      break
    default:
      return undefined
  }
  if (!to) return undefined
  return { id: to.id, chain: carries(to, followed) ? followed : chainOf(to) }
}

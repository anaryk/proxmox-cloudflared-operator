import { describe, expect, test } from 'vitest'

import type { RouteView, State, TrafficView } from '../api/types.gen'
import { collapse } from './collapse'
import { bands, cardFoot, cardHeader, layout, layoutWidth, portY, rowHeight, top } from './layout'
import { buildModel, type Model } from './model'
import chains from './testdata/chains.json'
import { scaled, trafficFor } from './testdata/scale'
import traffic from './testdata/traffic.json'
import type { Band, Layout } from './types'

const st = chains as unknown as State
const tv = traffic as unknown as TrafficView
const model = buildModel(st, tv)

const box = (l: Layout, id: string) => {
  const b = l.nodes.get(id)
  if (!b) throw new Error(`no box for ${id}`)
  return b
}

// The ids of a band, top to bottom.
const order = (m: Model, l: Layout, band: Band) =>
  m.nodes
    .filter((n) => n.band === band)
    .map((n) => n.id)
    .sort((a, b) => box(l, a).y - box(l, b).y)

const overlaps = (l: Layout) => {
  const out: string[] = []
  const all = [...l.nodes]
  for (const [a, x] of all) {
    for (const [b, y] of all) {
      if (a < b && x.x === y.x && x.y < y.y + y.height && y.y < x.y + x.height) out.push(`${a} ${b}`)
    }
  }
  return out
}

describe('layout', () => {
  const l = layout(model)

  test('five bands across 1244 units', () => {
    expect(l.width).toBe(layoutWidth)
    expect(bands).toEqual({
      hostnames: { x: 8, width: 300 },
      edge: { x: 350, width: 172 },
      connector: { x: 662, width: 168 },
      path: { x: 872, width: 134 },
      targets: { x: 1036, width: 200 },
    })
    for (const n of model.nodes) expect([box(l, n.id).x, box(l, n.id).width]).toEqual([bands[n.band].x, bands[n.band].width])
  })

  test('a zone card is its header and a row of 24 units per hostname', () => {
    expect([rowHeight, cardHeader]).toEqual([24, 36])
    const zone = model.nodes.find((n) => n.id === 'zone:example.com')
    expect(box(l, 'zone:example.com').height).toBe(cardHeader + (zone?.rows?.length ?? 0) * rowHeight + cardFoot)
  })

  test('cards do not overlap, and none is above the band titles', () => {
    expect(overlaps(l)).toEqual([])
    for (const b of l.nodes.values()) expect(b.y).toBeGreaterThanOrEqual(top)
    expect(l.height).toBeGreaterThan(Math.max(...[...l.nodes.values()].map((b) => b.y + b.height)))
  })

  test('the same model lays out the same way', () => {
    expect(layout(buildModel(st, tv))).toEqual(l)
  })

  test('an edge leaves its card on the right and ends at its access point, or at the card', () => {
    const shared = l.edges.get('path:vmbr0.20>guest:qemu/101|10.0.0.11:8080')
    const path = box(l, 'path:vmbr0.20')
    const web = box(l, 'guest:qemu/101')
    expect(shared).toEqual({ from: { x: path.x + path.width, y: path.y + path.height / 2 }, to: { x: web.x, y: portY(web.y, 0) } })
    const withdrawn = l.edges.get('path:vmbr1.20>guest:qemu/104#withdrawn')
    expect(withdrawn?.to).toEqual({ x: bands.targets.x, y: box(l, 'guest:qemu/104').y + 15 })
    expect(l.edges.size).toBe(model.edges.length)
  })

  test('the trunk is level: the connector sits beside its edge node', () => {
    const trunk = l.edges.get('edge:acc1>connector:acc1')
    expect(trunk?.from.y).toBe(trunk?.to.y)
  })

  test('a node sits by its neighbours: the edge node beside its zones, a path beside its guests', () => {
    // two sweeps put the tunnel of example.dev after that of example.com, as their zones are
    expect(order(model, l, 'edge').indexOf('edge:acc4')).toBeGreaterThan(order(model, l, 'edge').indexOf('edge:acc1'))
    const lab = box(l, 'guest:qemu/108')
    const vmbr1 = box(l, 'path:vmbr1')
    expect(vmbr1.y + vmbr1.height / 2).toBeCloseTo(lab.y + lab.height / 2)
  })
})

describe('stability', () => {
  const base = scaled({ routes: 30, zones: 3 })
  const before = buildModel(base, trafficFor(base))
  const first = layout(before)

  // one more route: a new guest on its own bridge, in the second zone
  const [template] = base.routes
  const added: RouteView = {
    ...(template as RouteView),
    hostname: 'new.zone-1.example',
    owner: 'qemu/2000',
    zone: 'zone-1.example',
    service: 'http://10.9.9.9:8080',
    guest: { kind: 'qemu', vmid: 2000, name: 'new' },
    path: { node: 'pve1', bridge: 'vmbr7', port: 'tap2000i0', verifiedAt: '2026-10-01T12:00:00Z' },
  }
  const more: State = { ...base, routes: [...base.routes, added] }
  const after = buildModel(more, trafficFor(more))
  const second = layout(after, first)

  test('the nodes there were keep their order in every band; the new ones are inserted', () => {
    for (const band of ['hostnames', 'edge', 'connector', 'path', 'targets'] as const) {
      const was = order(before, first, band)
      const now = order(after, second, band)
      expect(now.filter((id) => was.includes(id))).toEqual(was)
    }
    expect(order(after, second, 'path')).toContain('path:vmbr7')
    expect(order(after, second, 'targets')).toContain('guest:qemu/2000')
    expect(overlaps(second)).toEqual([])
  })

  test('a node that went keeps the others in their order', () => {
    const fewer: State = { ...base, routes: base.routes.slice(1) }
    const less = buildModel(fewer, trafficFor(fewer))
    const third = layout(less, first)
    expect(order(less, third, 'targets')).toEqual(order(before, first, 'targets').filter((id) => id !== 'guest:qemu/1000'))
  })

  test('a fresh layout of the same model is the same', () => {
    expect(layout(after)).toEqual(layout(after))
  })
})

test('a collapsed map lays out with its counts and groups', () => {
  const big = scaled({ routes: 500, zones: 2 })
  const { model: m } = collapse(buildModel(big, trafficFor(big)), { expanded: new Set() })
  const l = layout(m)
  const zone = m.nodes.find((n) => n.id === 'zone:example.com')
  expect(box(l, 'zone:example.com').height).toBe(cardHeader + (1 + (zone?.rows?.length ?? 0)) * rowHeight + cardFoot)
  expect(overlaps(l)).toEqual([])
  expect(l.edges.size).toBe(m.edges.length)
})

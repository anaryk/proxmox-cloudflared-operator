import { describe, expect, test } from 'vitest'

import type { State, TrafficView } from '../api/types.gen'
import { routeKey } from '../text/routes'
import { budget, collapse, expandAll, focusRoutes, type Level, levelOf, mapViewQuery, moreId, readMapView, routesOf } from './collapse'
import { buildModel, type Model } from './model'
import chains from './testdata/chains.json'
import { large, outage, scaled, trafficFor, wide } from './testdata/scale'
import traffic from './testdata/traffic.json'

const none = new Set<string>()

function view(st: State, opts: { previousLevel?: Level; focus?: string; expanded?: Set<string>; problems?: boolean } = {}) {
  const model = buildModel(st, trafficFor(st))
  return collapse(model, { expanded: none, ...opts })
}

const cards = (m: Model) => m.nodes.length
const rowsOf = (m: Model, id: string) => m.nodes.find((n) => n.id === id)?.rows ?? []
const within = (m: Model) => {
  expect(cards(m)).toBeLessThanOrEqual(budget.cards)
  expect(m.edges.length).toBeLessThanOrEqual(budget.edges)
}
// Every edge joins two nodes of the view.
const whole = (m: Model) => {
  const ids = new Set(m.nodes.map((n) => n.id))
  expect(m.edges.filter((e) => !ids.has(e.from) || !ids.has(e.to))).toEqual([])
}

describe('the level of a map', () => {
  test.each([
    [39, undefined, 'full'],
    [40, undefined, 'full'],
    [41, undefined, 'folded'],
    [200, undefined, 'folded'],
    [201, undefined, 'collapsed'],
    // left only 10 % below the limit
    [195, 'collapsed', 'collapsed'],
    [180, 'collapsed', 'collapsed'],
    [179, 'collapsed', 'folded'],
    [39, 'folded', 'folded'],
    [36, 'folded', 'folded'],
    [35, 'folded', 'full'],
    [30, 'collapsed', 'full'],
  ] as const)('%i routes after %s: %s', (n, previous, want) => {
    expect(levelOf(n, previous)).toBe(want)
  })

  test.each([
    [39, 'full'],
    [41, 'folded'],
    [201, 'collapsed'],
  ] as const)('a map of %i routes is %s', (n, want) => {
    expect(view(scaled({ routes: n })).level).toBe(want)
  })

  test('the guests counted are those drawn, not every guest the node lists', () => {
    // 2000 guests carry the tag; 30 of them name the 39 hostnames
    const st = { ...scaled({ routes: 39, guests: 30 }), gateTagged: 2000 }
    const { model, level } = view(st)
    expect(level).toBe('full')
    expect(model.nodes.filter((n) => n.kind === 'target')).toHaveLength(30)
  })

  test('near a limit the map keeps its level', () => {
    const first = view(scaled({ routes: 201 }))
    expect(first.level).toBe('collapsed')
    expect(view(scaled({ routes: 195 }), { previousLevel: first.level }).level).toBe('collapsed')
    expect(view(scaled({ routes: 179 }), { previousLevel: first.level }).level).toBe('folded')
  })
})

describe('full', () => {
  test('everything as it is', () => {
    const st = scaled({ routes: 39 })
    const model = buildModel(st, trafficFor(st))
    expect(collapse(model, { expanded: none }).model).toEqual(model)
  })
})

describe('folded', () => {
  const st = scaled({ routes: 150, zones: 2, down: (i) => (i % 50 === 6 ? { state: 'unreachable', reason: 'target is not answering' } : undefined) })
  const { model, level } = view(st)

  test('a zone card shows 12 rows, the problems first, and folds the rest', () => {
    expect(level).toBe('folded')
    const rows = rowsOf(model, 'zone:example.com')
    expect(rows).toHaveLength(13)
    expect(rows.slice(0, 3).map((r) => r.state)).toEqual(['unreachable', 'unreachable', 'unreachable'])
    expect(rows.slice(3, 12).every((r) => r.state === 'active')).toBe(true)
    expect(rows[12]).toMatchObject({ id: moreId('zone:example.com'), kind: 'more', label: '+ 63 active', state: 'active' })
    expect(rows[12]?.routes).toHaveLength(63)
  })

  test('the guests whose routes are all active fold per path', () => {
    const groups = model.nodes.filter((n) => n.kind === 'group')
    expect(groups.map((n) => [n.id, n.label])).toEqual([
      ['guests:path:vmbr0|active', '110 guests on vmbr0'],
      ['guests:path:vmbr1.20|active', '37 guests on vmbr1 · VLAN 20'],
    ])
    // the guests of the problems keep their cards and their chains
    expect(model.nodes.filter((n) => n.kind === 'target').map((n) => n.id)).toEqual(['guest:qemu/1006', 'guest:qemu/1056', 'guest:qemu/1106'])
    const into = model.edges.find((e) => e.to === 'guests:path:vmbr0|active')
    expect(into).toMatchObject({ from: 'path:vmbr0', style: 'plain' })
    expect(into?.routes).toHaveLength(110)
    within(model)
    whole(model)
  })

  test('the folded rows and groups open', () => {
    const opened = view(st, { expanded: new Set([moreId('zone:example.com'), 'guests:path:vmbr1.20|active']) }).model
    expect(rowsOf(opened, 'zone:example.com')).toHaveLength(75)
    expect(rowsOf(opened, 'zone:zone-1.example')).toHaveLength(13)
    expect(opened.nodes.some((n) => n.id === 'guests:path:vmbr1.20|active')).toBe(false)
    expect(opened.nodes.filter((n) => n.kind === 'target')).toHaveLength(40)
    const all = view(scaled({ routes: 60, zones: 2 }), { expanded: new Set([expandAll]) }).model
    expect(all.nodes.filter((n) => n.kind === 'group')).toEqual([])
    expect(rowsOf(all, 'zone:example.com')).toHaveLength(30)
  })

  test("a group's edge sums the fresh figures of its guests only", () => {
    const tv = trafficFor(st)
    const figures = tv.routes.map((f, i) => ({ ...f, flowsPerSec: f.flowsPerSec + 1, stale: i % 2 === 1 }))
    const into = (stale: (f: (typeof figures)[number]) => boolean) => {
      const m = buildModel(st, { ...tv, routes: figures.map((f) => ({ ...f, stale: stale(f) })) })
      return collapse(m, { expanded: none }).model.edges.find((e) => e.to === 'guests:path:vmbr0|active')
    }
    const some = into((f) => f.stale)
    const carried = new Set(some?.routes)
    const fresh = new Map(figures.filter((f) => !f.stale && carried.has(routeKey(f))).map((f) => [f.target, f.flowsPerSec]))
    expect(fresh.size).toBeGreaterThan(0)
    expect(some?.rate).toBeCloseTo([...fresh.values()].reduce((a, b) => a + b, 0))
    expect(some?.stale).toBeUndefined()
    expect(into(() => true)?.stale).toBe(true)
  })

  test('what is opened still keeps to the budget', () => {
    const all = view(st, { expanded: new Set([expandAll]) }).model
    within(all)
    whole(all)
    expect(rowsOf(all, 'zone:example.com')).toHaveLength(75)
  })
})

describe('collapsed', () => {
  test('zone cards are counts and problems; the guests fold per path', () => {
    const st = scaled({ routes: 300, zones: 3, down: (i) => ([0, 99, 198].includes(i) ? { state: 'withdrawn', reason: 'identity check failed' } : undefined) })
    const { model, level } = view(st)
    expect(level).toBe('collapsed')
    const zone = model.nodes.find((n) => n.id === 'zone:example.com')
    expect(zone?.counts).toEqual({ active: 97, withdrawn: 3 })
    expect(zone?.rows?.map((r) => r.kind === 'more' ? r.label : `${r.hostname} ${r.state}`)).toEqual([
      'app-0000.example.com withdrawn',
      'app-0099.example.com withdrawn',
      'app-0198.example.com withdrawn',
      '+ 97 active',
    ])
    expect(model.nodes.filter((n) => n.kind === 'target')).toHaveLength(3)
    within(model)
    whole(model)
  })

  test('without problems first, counts only and every guest folded', () => {
    const st = scaled({ routes: 300, down: (i) => (i % 100 === 1 ? { state: 'withdrawn', reason: 'identity check failed' } : undefined) })
    const { model } = view(st, { problems: false })
    expect(rowsOf(model, 'zone:example.com').map((r) => r.label)).toEqual(['+ 300 more'])
    expect(model.nodes.filter((n) => n.kind === 'target')).toEqual([])
    expect(model.nodes.filter((n) => n.kind === 'group').map((n) => n.label)).toEqual([
      '222 guests on vmbr0',
      '3 guests on vmbr0',
      '75 guests on vmbr1 · VLAN 20',
    ])
  })

  test('an outage is grouped by zone, state and reason, within the budget', () => {
    const st = outage()
    const { model, level } = view(st)
    expect(level).toBe('collapsed')
    within(model)
    whole(model)
    const groups = rowsOf(model, 'zone:example.com').filter((r) => r.kind === 'group')
    expect(groups.map((r) => r.label)).toEqual([
      '67 unreachable: target is not answering',
      '67 unreachable: no verified address yet',
      '66 unreachable: address was verified for another owner',
    ])
    // every unreachable route is in one group, which lists them
    const grouped = model.nodes.flatMap((n) => (n.rows ?? []).filter((r) => r.kind === 'group').flatMap((r) => r.routes ?? []))
    expect(new Set(grouped).size).toBe(600)
    expect(model.nodes.filter((n) => n.kind === 'target')).toEqual([])
    expect(model.edges.filter((e) => e.style === 'unreachable').map((e) => e.to)).toEqual(['guests:path:vmbr0|unreachable', 'guests:path:vmbr1.20|unreachable'])
  })

  test('near 240 problem rows the grouping holds, until they are 10 % fewer', () => {
    // rejected routes have rows and no edges, so only the rows count
    const down = (n: number) => (i: number) => (i < n ? { state: 'rejected', reason: i % 2 === 0 ? 'not named by allowHosts' : 'an apex' } : undefined)
    const grouped = (n: number, previousGrouped?: boolean) => {
      const st = scaled({ routes: 300, zones: 3, down: down(n) })
      const v = collapse(buildModel(st, trafficFor(st)), { expanded: none, previousGrouped })
      const groups = v.model.nodes.flatMap((node) => (node.rows ?? []).filter((r) => r.kind === 'group'))
      expect(groups.length > 0).toBe(v.grouped)
      within(v.model)
      return v.grouped
    }
    expect(grouped(230)).toBe(false)
    expect(grouped(241)).toBe(true)
    expect(grouped(230, true)).toBe(true)
    expect(grouped(216, true)).toBe(true)
    expect(grouped(215, true)).toBe(false)
  })

  test('an opened group stays a group on the map; the chain list has its routes', () => {
    const st = outage()
    const { model } = view(st)
    const group = rowsOf(model, 'zone:example.com').find((r) => r.kind === 'group')
    if (!group?.id) throw new Error('no group')
    const opened = view(st, { expanded: new Set([group.id]) }).model
    expect(rowsOf(opened, 'zone:example.com').find((r) => r.id === group.id)).toEqual(group)
    const routes = routesOf(opened, group.id)
    expect(routes).toHaveLength(67)
    expect(new Set(routes.map((k) => st.routes.find((r) => routeKey(r) === k)?.reason))).toEqual(new Set(['target is not answering']))
    expect(routesOf(opened, 'zone:example.com')).toHaveLength(334)
    expect(routesOf(opened, 'nothing')).toEqual([])
  })

  test.each([
    ['large', large],
    ['wide', wide],
  ])('%s stays within the budget', (_, make) => {
    const { model, level } = view(make())
    expect(level).toBe('collapsed')
    within(model)
    whole(model)
  })

  test('very many zones fold into one card', () => {
    const { model } = view(scaled({ routes: 400, zones: 300, accounts: 2 }))
    within(model)
    whole(model)
    const folded = model.nodes.find((n) => n.id === 'zones:more')
    expect(folded?.label).toMatch(/^\d+ more zones$/)
    expect(model.edges.filter((e) => e.from === 'zones:more').map((e) => e.to)).toEqual(['edge:acc-1', 'edge:acc1'])
  })
})

describe('focus', () => {
  const st = scaled({ routes: 1000, zones: 4 })
  const model = buildModel(st, trafficFor(st))

  test('a guest: its chain whole, whatever the size of the map', () => {
    const { model: m, level } = collapse(model, { expanded: none, focus: 'guest:qemu/1005' })
    expect(level).toBe('collapsed')
    expect(m.nodes.map((n) => n.id)).toEqual(['zone:zone-1.example', 'edge:acc1', 'connector:acc1', 'path:vmbr0', 'guest:qemu/1005'])
    expect(rowsOf(m, 'zone:zone-1.example').map((r) => r.hostname)).toEqual(['app-0005.zone-1.example'])
    expect(m.edges.map((e) => [e.id, e.label])).toEqual([
      ['connector:acc1>path:vmbr0', undefined],
      ['edge:acc1>connector:acc1', undefined],
      ['path:vmbr0>guest:qemu/1005|10.3.237.10:8080', undefined],
      ['zone:zone-1.example>edge:acc1', '1 rule'],
    ])
  })

  test('a hostname, a zone, a tunnel and words', () => {
    expect([...focusRoutes(model, 'hostname:app-0007.zone-3.example')]).toEqual([routeKey({ hostname: 'app-0007.zone-3.example', owner: 'qemu/1007' })])
    expect(focusRoutes(model, 'zone:example.com').size).toBe(250)
    expect(focusRoutes(model, 'tunnel:acc1').size).toBe(1000)
    expect([...focusRoutes(model, 'APP-0999')]).toEqual([routeKey({ hostname: 'app-0999.zone-3.example', owner: 'qemu/1999' })])
    expect(focusRoutes(model, 'nothing like it').size).toBe(0)
  })

  test('a zone of 250 routes is folded as 250 routes are', () => {
    const { model: m } = collapse(model, { expanded: none, focus: 'zone:example.com' })
    expect(m.nodes.filter((n) => n.kind === 'zone').map((n) => n.id)).toEqual(['zone:example.com'])
    expect(m.nodes.find((n) => n.id === 'zone:example.com')?.counts).toEqual({ active: 250 })
    within(m)
  })

  test('a tunnel keeps the connectors pco does not run', () => {
    const m = buildModel(chains as unknown as State, traffic as unknown as TrafficView)
    const { model: v } = collapse(m, { expanded: none, focus: 'tunnel:acc1' })
    expect(v.nodes.filter((n) => n.kind === 'rogue')).toHaveLength(1)
    expect(v.edges.filter((e) => e.style === 'rogue')).toHaveLength(1)
    expect(v.nodes.some((n) => n.id === 'edge:acc4')).toBe(false)
  })
})

test('the view in the address bar', () => {
  const v = readMapView('?focus=guest%3Aqemu%2F101&expand=more%3Azone%3Aexample.com&expand=guests%3Apath%3Avmbr0%7Cactive&problems=0&tab=x')
  expect(v).toEqual({ focus: 'guest:qemu/101', expanded: new Set(['more:zone:example.com', 'guests:path:vmbr0|active']), problems: false })
  const q = mapViewQuery('?tab=x&focus=old', v)
  expect(q).toBe('?tab=x&focus=guest%3Aqemu%2F101&expand=guests%3Apath%3Avmbr0%7Cactive&expand=more%3Azone%3Aexample.com&problems=0')
  expect(readMapView(q)).toEqual(v)
  expect(mapViewQuery('?focus=x', { expanded: new Set() })).toBe('')
})

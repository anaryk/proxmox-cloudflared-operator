import { describe, expect, test } from 'vitest'

import type { State, TrafficView } from '../api/types.gen'
import first from '../fixtures/first-run.json'
import populated from '../fixtures/populated.json'
import rogueFixture from '../fixtures/rogue.json'
import trafficFixture from '../fixtures/traffic.json'
import { routeKey } from '../text/routes'
import { buildChains, buildModel, type FlowEdge, type FlowNode, pathOf, targetOf, trunksOf } from './model'
import chains from './testdata/chains.json'
import traffic from './testdata/traffic.json'

const st = chains as unknown as State
const tv = traffic as unknown as TrafficView
const model = buildModel(st, tv)

const node = (id: string): FlowNode => {
  const n = model.nodes.find((x) => x.id === id)
  if (!n) throw new Error(`no node ${id}`)
  return n
}
const edge = (id: string): FlowEdge => {
  const e = model.edges.find((x) => x.id === id)
  if (!e) throw new Error(`no edge ${id}: ${model.edges.map((x) => x.id).join(', ')}`)
  return e
}
const carrying = (key: string) => model.edges.filter((e) => e.routes?.includes(key))
const rowOf = (zone: string, hostname: string, owner: string) => node(zone).rows?.find((r) => r.hostname === hostname && r.owner === owner)

describe('the model of a state with a route of every kind', () => {
  test('is the golden', async () => {
    await expect(`${JSON.stringify(model, null, 2)}\n`).toMatchFileSnapshot('testdata/chains.model.json')
  })

  test('a node per zone with hostnames, per tunnel, per connector, per path and per target', () => {
    const ids = (band: string) => model.nodes.filter((n) => n.band === band).map((n) => n.id)
    expect(ids('hostnames')).toEqual(['no-zone', 'zone:example.com', 'zone:example.dev', 'zone:example.info'])
    expect(ids('edge')).toEqual(['edge:acc1', 'edge:acc2', 'edge:acc3', 'edge:acc4'])
    expect(ids('connector')).toEqual(['connector:acc1', 'connector:acc4', 'rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807'])
    expect(ids('path')).toEqual(['path:none', 'path:vmbr0', 'path:vmbr0.20', 'path:vmbr1', 'path:vmbr1.20'])
    expect(ids('targets')).toEqual(['address:10.0.9.5', 'guest:qemu/101', 'guest:qemu/103', 'guest:qemu/104', 'guest:qemu/106', 'guest:qemu/108'])
    expect(node('zone:example.com')).toMatchObject({ kind: 'zone', label: 'example.com', state: 'served', lines: ['Main'] })
    expect(node('zone:example.info').state).toBe('frozen')
  })

  test('two tunnels of the same name are told apart by their accounts', () => {
    expect(st.tunnels.every((t) => t.name === 'pco-abc123')).toBe(true)
    expect(node('edge:acc1').label).toBe('Main · 00000000')
    expect(node('edge:acc4').label).toBe('Lab · 00000000')
    expect(node('edge:acc2').label).toBe('acc2')
    expect(node('edge:acc1')).toMatchObject({ state: 'yes', lines: ['4 connections', 'fra08 · prg01', 'RTT 11–19 ms'] })
    expect(node('edge:acc4').state).toBe('unchecked')
  })

  test('the connector in its words, tagged when its tunnel was not checked', () => {
    expect(node('connector:acc1')).toMatchObject({ label: 'pve1', state: 'active, ready, 4 connections', lines: ['Main · 00000000', 'config v3 · rolled out', 'cloudflared 2026.9.3'] })
    expect(node('connector:acc1').tags).toBeUndefined()
    expect(node('connector:acc4').tags).toEqual(['unchecked'])
  })

  test('the trunk: the lanes of the connections, the rate and errors of the last sample', () => {
    const last = tv.tunnels[0]?.samples.at(-1)
    expect(edge('edge:acc1>connector:acc1')).toMatchObject({ style: 'trunk', lanes: 4, rate: last?.rps, errors: last?.errorsPerSec })
    expect(edge('edge:acc1>connector:acc1').stale).toBeUndefined()
    // a stale sample and a tunnel not checked: greyed, and its figure is old
    expect(edge('edge:acc4>connector:acc4')).toMatchObject({ style: 'trunk', lanes: 2, rate: 4, stale: true, muted: true })
  })

  test('hostname edges are bundled per zone card and counted, and never carry a figure', () => {
    const e = edge('zone:example.com>edge:acc1')
    expect(e).toMatchObject({ style: 'hairline', label: '6 rules' })
    expect(e.rate).toBeUndefined()
    expect(e.routes).toEqual(
      ['api.example.com qemu/103', 'app.example.com qemu/101', 'held.example.com lxc/200', 'intranet.example.com manual/m1', 'old.example.com qemu/104', 'www.example.com qemu/101'],
    )
  })

  test('a target shared by two routes has one access point and one figure', () => {
    const web = node('guest:qemu/101')
    expect(web.ports).toEqual([
      { id: 'guest:qemu/101|10.0.0.11:8080', label: ':8080 http', state: 'active', target: '10.0.0.11:8080', level: 'port', rate: 2.4, routes: ['app.example.com qemu/101', 'www.example.com qemu/101'] },
    ])
    expect(edge('path:vmbr0.20>guest:qemu/101|10.0.0.11:8080')).toMatchObject({ style: 'plain', to: 'guest:qemu/101', port: 'guest:qemu/101|10.0.0.11:8080', rate: 2.4 })
    // the connector -> path edge sums the targets behind it, each once
    expect(edge('connector:acc1>path:vmbr0.20').rate).toBe(2.4)
    expect(edge('connector:acc1>path:none').rate).toBe(0.6)
  })

  test('the VLAN path node and the one of no bridge proven', () => {
    expect(node('path:vmbr0.20').label).toBe('vmbr0 · VLAN 20 · direct')
    expect(node('path:vmbr0').label).toBe('vmbr0 · direct')
    expect(node('path:none').label).toBe('no bridge proven')
    expect(node('path:none').routes).toEqual(['intranet.example.com manual/m1'])
    expect(node('address:10.0.9.5')).toMatchObject({ kind: 'target', label: '10.0.9.5', lines: ['manual/m1'] })
  })

  test('unreachable, withdrawn and frozen keep their chains in their styles', () => {
    expect(edge('path:vmbr0>guest:qemu/103|10.0.0.13:9000').style).toBe('unreachable')
    // a withdrawn route has no address that passed: its edge ends at the card
    expect(edge('path:vmbr1.20>guest:qemu/104#withdrawn')).toMatchObject({ style: 'withdrawn', label: '503' })
    const frozen = carrying(routeKey({ hostname: 'www.example.info', owner: 'qemu/106' }))
    expect(frozen.map((e) => e.id)).toEqual(['path:vmbr0>guest:qemu/106#plain', 'zone:example.info>edge:acc3'])
    expect(frozen.every((e) => e.muted)).toBe(true)
  })

  test('held: a 503 tag, its rule in the tunnel and nothing past the connector', () => {
    const key = routeKey({ hostname: 'held.example.com', owner: 'lxc/200' })
    expect(rowOf('zone:example.com', 'held.example.com', 'lxc/200')?.tags).toEqual(['503'])
    expect(carrying(key).map((e) => e.style)).toEqual(['trunk', 'hairline'])
  })

  test('rejected, in conflict and without a zone: a row and no edge at all', () => {
    for (const [zone, hostname, owner] of [
      ['zone:example.com', 'example.com', 'qemu/105'],
      ['zone:example.com', 'www.example.com', 'qemu/102'],
      ['no-zone', 'shop.example.net', 'qemu/107'],
    ] as const) {
      expect(rowOf(zone, hostname, owner)).toBeTruthy()
      expect(carrying(routeKey({ hostname, owner }))).toEqual([])
    }
    expect(rowOf('zone:example.com', 'example.com', 'qemu/105')).toMatchObject({ state: 'rejected', reason: expect.stringContaining('add "example.com" to allowHosts') })
    expect(rowOf('zone:example.com', 'www.example.com', 'qemu/102')).toMatchObject({ state: 'conflict', holder: 'qemu/101 web-1' })
  })

  test('the hostnames of guests waiting for approval, marked and without edges', () => {
    const rows = node('zone:example.com').rows?.filter((r) => r.kind === 'unapproved')
    expect(rows).toEqual([
      { id: 'route:dns.example.com lxc/202', kind: 'unapproved', hostname: 'dns.example.com', owner: 'lxc/202', guest: 'dns-1', state: 'unapproved', reason: 'delegated: alice@pve holds VM.Config.Network; address 10.0.0.1 is the gateway of node pve1', tags: ['waits for approval'] },
      { id: 'route:new.example.com lxc/201', kind: 'unapproved', hostname: 'new.example.com', owner: 'lxc/201', guest: 'new-1', state: 'unapproved', reason: 'admission mode approve', tags: ['waits for approval'] },
    ])
    expect(carrying(routeKey({ hostname: 'new.example.com', owner: 'lxc/201' }))).toEqual([])
  })

  test('a name a record of someone else holds carries the DNS tag', () => {
    expect(rowOf('zone:example.com', 'api.example.com', 'qemu/103')?.tags).toEqual(['DNS'])
  })

  test('a row is ordered by hostname, then by owner as the command line orders them', () => {
    const rows = node('zone:example.com').rows ?? []
    expect(rows.map((r) => `${r.hostname} ${r.owner}`)).toEqual([
      'api.example.com qemu/103',
      'app.example.com qemu/101',
      'dns.example.com lxc/202',
      'example.com qemu/105',
      'held.example.com lxc/200',
      'intranet.example.com manual/m1',
      'new.example.com lxc/201',
      'old.example.com qemu/104',
      'www.example.com qemu/101',
      'www.example.com qemu/102',
    ])
  })
})

test('the connectors pco does not run: a red node each, joined to their tunnel, never moving', () => {
  const m = buildModel(rogueFixture as unknown as State, trafficFixture as unknown as TrafficView)
  const rogues = m.nodes.filter((n) => n.kind === 'rogue')
  expect(rogues.map((n) => [n.id, n.band, n.label])).toEqual([
    ['rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807', 'connector', 'not run by pco: 198.51.100.7'],
    ['rogue:7a2c4e91-5d3b-4f80-9e16-2b8d0c5a7f43', 'connector', 'not run by pco: 203.0.113.9'],
  ])
  const edges = m.edges.filter((e) => e.style === 'rogue')
  expect(edges.map((e) => [e.from, e.to])).toEqual(rogues.map((n) => ['edge:acc1', n.id]))
  expect(edges.every((e) => e.rate === undefined)).toBe(true)
})

test('without the egress counters the edges past the connector have no figure; the trunk keeps its own', () => {
  const m = buildModel(st, { ...tv, routesWhy: 'the egress filter is off: pco has no per-guest counters' })
  expect(m.edges.filter((e) => e.style !== 'trunk' && e.rate !== undefined)).toEqual([])
  expect(m.nodes.flatMap((n) => n.ports ?? []).filter((p) => p.rate !== undefined)).toEqual([])
  expect(m.edges.find((e) => e.style === 'trunk')?.rate).toBeGreaterThan(0)
})

test('before the first cycle there is nothing to draw', () => {
  expect(buildModel(first as unknown as State, undefined)).toEqual({ nodes: [], edges: [] })
  expect(buildChains(first as unknown as State, undefined)).toEqual([])
})

test('the populated golden draws its one route and the route that lost the name', () => {
  const m = buildModel(populated as unknown as State, trafficFixture as unknown as TrafficView)
  expect(m.nodes.find((n) => n.id === 'zone:example.com')?.rows?.map((r) => [r.hostname, r.owner, r.state])).toEqual([
    ['dns.example.com', 'lxc/202', 'unapproved'],
    ['new.example.com', 'lxc/201', 'unapproved'],
    ['www.example.com', 'qemu/101', 'active'],
    ['www.example.com', 'qemu/102', 'conflict'],
  ])
  expect(m.edges.find((e) => e.to === 'guest:qemu/101')?.rate).toBe(2.4)
})

test('targets and paths', () => {
  expect(targetOf('http://10.0.0.11:8080')).toEqual({ scheme: 'http', host: '10.0.0.11', port: '8080', target: '10.0.0.11:8080' })
  expect(targetOf('https://[fd00::5]')).toEqual({ scheme: 'https', host: '[fd00::5]', port: '443', target: '[fd00::5]:443' })
  expect(targetOf('http_status:503')).toBeUndefined()
  expect(targetOf('')).toBeUndefined()
  expect(pathOf({ path: { node: 'pve1', verifiedAt: '' } })).toEqual({ id: 'path:none', label: 'no bridge proven' })
})

describe('the chains of the list', () => {
  const list = buildChains(st, tv)
  const of = (hostname: string, owner: string) => {
    const c = list.find((x) => x.hostname === hostname && x.owner === owner)
    if (!c) throw new Error(`no chain ${hostname} ${owner}`)
    return c
  }
  const steps = (hostname: string, owner: string) => of(hostname, owner).steps.map((s) => `${s.name} ${s.level}: ${s.text}`)

  test('one per hostname, routes and those waiting for approval, by hostname', () => {
    expect(list.map((c) => c.key)).toEqual([
      'api.example.com qemu/103',
      'app.example.com qemu/101',
      'dns.example.com lxc/202',
      'example.com qemu/105',
      'held.example.com lxc/200',
      'intranet.example.com manual/m1',
      'lab.example.dev qemu/108',
      'new.example.com lxc/201',
      'old.example.com qemu/104',
      'shop.example.net qemu/107',
      'www.example.com qemu/101',
      'www.example.com qemu/102',
      'www.example.info qemu/106',
    ])
  })

  test('active: the whole chain and the figure of its shared target', () => {
    expect(steps('www.example.com', 'qemu/101')).toEqual([
      'Zone ok: example.com · served · Main',
      'Edge ok: Main · 00000000 · verified: yes',
      'Connector ok: pve1 · active, ready, 4 connections',
      'Path ok: vmbr0 · VLAN 20 · direct',
      'Target ok: qemu/101 web-1 · 10.0.0.11:8080 http · level port',
    ])
    expect(of('www.example.com', 'qemu/101').figure).toEqual({ target: '10.0.0.11:8080', rate: 2.4, shared: 2, stale: false })
  })

  test('each state ends where its chain ends', () => {
    expect(steps('api.example.com', 'qemu/103')).toEqual([
      'Zone ok: example.com · served · Main',
      'DNS fail: a record of someone else holds the name: A 192.0.2.10',
      'Edge ok: Main · 00000000 · verified: yes',
      'Connector ok: pve1 · active, ready, 4 connections',
      'Path ok: vmbr0 · direct',
      'Target fail: qemu/103 api-1 · 10.0.0.13:9000 http · target is not answering',
    ])
    expect(steps('old.example.com', 'qemu/104').at(-1)).toBe('Target warn: qemu/104 db-1 · withdrawn (503): identity check failed')
    expect(steps('held.example.com', 'lxc/200').at(-1)).toBe('Rule warn: 503: named in the Notes of lxc/200 but not routed; claim kept')
    expect(steps('www.example.info', 'qemu/106')).toEqual([
      'Zone warn: example.info · frozen: zone example.info is no longer listed by credential cred1',
      'Edge warn: acc3 · 00000000 · verified: held: account frozen: zone example.info is no longer listed by credential cred1',
      'Connector fail: no connector on this node',
      'Rule warn: account frozen: zone example.info is no longer listed by credential cred1',
    ])
    expect(steps('example.com', 'qemu/105')).toEqual([
      'Zone ok: example.com · served · Main',
      'Hostname policy warn: the apex of zone example.com is published only when allowHosts names it: add "example.com" to allowHosts',
    ])
    expect(steps('www.example.com', 'qemu/102')).toEqual(['Zone ok: example.com · served · Main', 'Claim fail: hostname is held by qemu/101'])
    expect(steps('shop.example.net', 'qemu/107')).toEqual(['Zone fail: zone example.net is served through no credential'])
    expect(steps('new.example.com', 'lxc/201')).toEqual(['Zone ok: example.com · served · Main', 'Approval info: waits for approval: admission mode approve'])
    expect(steps('intranet.example.com', 'manual/m1').slice(-2)).toEqual(['Path info: no bridge proven', 'Target ok: manual/m1 · 10.0.9.5:80 http · level manual'])
    expect(steps('lab.example.dev', 'qemu/108').slice(1, 3)).toEqual(['Edge warn: Lab · 00000000 · verified: unchecked: not checked in the last cycle: no writer identity; run pco setup', 'Connector ok: pve1 · active, ready, 2 connections'])
    expect(of('lab.example.dev', 'qemu/108').figure).toEqual({ target: '10.0.2.8:8080', rate: 1.2, shared: 1, stale: true })
  })

  test('the trunks: each tunnel with a connector, its last figures', () => {
    expect(trunksOf(st, tv).map((t) => [t.label, t.rps, t.stale, t.ready])).toEqual([
      ['Main · 00000000', tv.tunnels[0]?.samples.at(-1)?.rps, false, true],
      ['Lab · 00000000', 4, true, true],
    ])
  })
})

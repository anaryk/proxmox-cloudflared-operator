import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import type { NodeView, State, TrafficView, TunnelView } from '../api/types.gen'
import { ToastProvider } from '../components/Toast'
import cluster from '../fixtures/cluster.json'
import { eventsOf } from '../pages/Overview'
import { fakeStore, flush } from '../test/store'
import { nodeLabel } from '../text/flow'
import { routeKey } from '../text/routes'
import { collapse, expandAll, focusRoutes, foldedReplicas, foldReplicas, replicasId } from './collapse'
import { FlowDrawer } from './FlowDrawer'
import FlowMap from './FlowMap'
import { layout } from './layout'
import { buildChains, buildModel, type FlowEdge, type FlowNode, pinnedEdgeId, replicaNodeId, replicaOf, replicaState, tunnelNodeId, tunnelOfEdge } from './model'
import { createMotion, dotCap } from './motion'
import { extrasOf } from './panel'
import { trafficFor } from './testdata/scale'

// The flow map on a cluster: the connector band holds a replica per node
// and tunnel, a tunnel pinned to a node has an edge of its own, the chain
// list says how each replica fares, and above six replicas they fold.

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const st = cluster as unknown as State
const tv = trafficFor(st)
const model = buildModel(st, tv)

const node = (id: string): FlowNode => {
  const n = model.nodes.find((x) => x.id === id)
  if (!n) throw new Error(`no node ${id}: ${model.nodes.map((x) => x.id).join(', ')}`)
  return n
}
const edge = (id: string): FlowEdge => {
  const e = model.edges.find((x) => x.id === id)
  if (!e) throw new Error(`no edge ${id}: ${model.edges.map((x) => x.id).join(', ')}`)
  return e
}
const carrying = (key: string) => model.edges.filter((e) => e.routes?.includes(key)).map((e) => e.id)

describe('the model of a cluster', () => {
  test('a replica per node for the shared tunnel this node runs, and an edge and a replica for a tunnel pinned to a node', () => {
    const ids = (band: string) => model.nodes.filter((n) => n.band === band).map((n) => n.id)
    expect(ids('edge')).toEqual(['edge:acc1', 'edge:acc1:pve2', 'edge:acc2', 'edge:acc3', 'edge:acc4'])
    expect(ids('connector')).toEqual(['replica:acc1:pve1', 'replica:acc1:pve2', 'replica:acc1:pve2:pinned', 'replica:acc1:pve3', 'rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807'])
    expect(node('edge:acc1:pve2')).toMatchObject({ kind: 'edge', label: 'pinned to pve2 · Main', state: 'yes', ref: 'acc1', lines: ['connector on pve2'] })
    expect(node('replica:acc1:pve1')).toMatchObject({ kind: 'replica', label: 'pve1', state: 'ready', ref: 'pve1', lines: ['Main · 00000000', '9 verified · 1 failing'] })
    expect(node('replica:acc1:pve2')).toMatchObject({ state: 'ready', lines: ['Main · 00000000', '10 verified · 1 failing · 1 pinned'] })
    expect(node('replica:acc1:pve3')).toMatchObject({ state: 'joining', lines: ['Main · 00000000', '0 verified'] })
    expect(node('replica:acc1:pve2:pinned')).toMatchObject({ kind: 'replica', label: 'pve2', state: 'ready', lines: ['pinned to pve2 · Main', '10 verified · 1 failing · 1 pinned'] })
    // no card of this node alone
    expect(model.nodes.some((n) => n.kind === 'connector')).toBe(false)
  })

  // The node card of a pinned tunnel goes by the connector of that tunnel,
  // as the node's agent reports it, and not by the routes it reaches: an
  // agent that verifies the targets but runs no connector is not ready.
  test('the node of a tunnel pinned to another node is ready only when its agent reports that connector', () => {
    const silent: State = { ...st, nodes: st.nodes.map((n) => (n.name === 'pve2' ? { ...n, pinnedReady: [] } : n)) }
    const m = buildModel(silent, tv)
    expect(m.nodes.find((n) => n.id === 'replica:acc1:pve2:pinned')).toMatchObject({ state: 'not ready' })
    expect(m.nodes.find((n) => n.id === 'replica:acc1:pve2')).toMatchObject({ state: 'ready' })
  })

  test('a tunnel pinned to this node shows its own connector on its edge, and its node goes by it', () => {
    const pinnedID = '00000000-0000-4000-8000-000000000020'
    const here: State = { ...st, node: 'pve2', connectors: [...st.connectors, { tunnelId: pinnedID, active: true, ready: true, connections: 2 }] }
    const m = buildModel(here, tv)
    expect(m.nodes.find((n) => n.id === 'edge:acc1:pve2')).toMatchObject({ lines: ['2 connections'] })
    expect(m.nodes.find((n) => n.id === 'replica:acc1:pve2:pinned')).toMatchObject({ state: 'ready' })
    const down: State = { ...here, connectors: st.connectors }
    expect(buildModel(down, tv).nodes.find((n) => n.id === 'replica:acc1:pve2:pinned')).toMatchObject({ state: 'not ready' })
  })

  // With every route of an account pinned to a node, the shared tunnel
  // serves nothing and is not drawn, nor are its replicas; the tunnel
  // pinned to pve2 and its node are the chain.
  test('a shared tunnel that serves nothing while pinned tunnels serve is not drawn', () => {
    const allPinned: State = {
      ...st,
      rogueConnectors: [],
      routes: st.routes
        .filter((r) => r.accountId !== 'acc1' || r.placement)
        .map((r) =>
          r.placement?.kind === 'shared'
            ? { ...r, placement: { tunnel: 'pco-abc123-pve2', kind: 'pinned', node: 'pve2' }, replicas: r.replicas.filter((rv) => rv.node === 'pve2') }
            : r,
        ),
    }
    const ids = buildModel(allPinned, tv).nodes.map((n) => n.id)
    expect(ids).not.toContain('edge:acc1')
    expect(ids).not.toContain('replica:acc1:pve1')
    expect(ids).not.toContain('replica:acc1:pve2')
    expect(ids).toContain('edge:acc1:pve2')
    expect(ids).toContain('replica:acc1:pve2:pinned')
  })

  test("the trunk of this node carries its figures; the others' have one lane and none", () => {
    const own = edge('edge:acc1>replica:acc1:pve1')
    expect(own).toMatchObject({ style: 'trunk', lanes: 4 })
    expect(own.rate).toBeGreaterThan(0)
    expect(edge('edge:acc1>replica:acc1:pve2')).toMatchObject({ style: 'trunk', lanes: 1 })
    expect(edge('edge:acc1>replica:acc1:pve2').rate).toBeUndefined()
    expect(edge('edge:acc1:pve2>replica:acc1:pve2:pinned')).toMatchObject({ style: 'trunk', lanes: 1 })
  })

  test('a shared route runs through every replica of its tunnel to its path, with the figure on this node’s line alone', () => {
    const key = routeKey({ hostname: 'www.example.com', owner: 'qemu/101' })
    expect(carrying(key)).toEqual([
      'edge:acc1>replica:acc1:pve1',
      'edge:acc1>replica:acc1:pve2',
      'path:vmbr0.20>guest:qemu/101|10.0.0.11:8080',
      'replica:acc1:pve1>path:vmbr0.20',
      'replica:acc1:pve2>path:vmbr0.20',
      'zone:example.com>edge:acc1',
    ])
    expect(edge('replica:acc1:pve1>path:vmbr0.20').rate).toBeGreaterThan(0)
    expect(edge('replica:acc1:pve2>path:vmbr0.20').rate).toBeUndefined()
    expect(edge('replica:acc1:pve2>path:vmbr0.20').style).toBe('plain')
    expect(node('replica:acc1:pve2').routes).toContain(key)
  })

  test('a route pinned to a node goes through its pinned edge and that node alone', () => {
    const key = routeKey({ hostname: 'git.example.com', owner: 'qemu/110' })
    expect(carrying(key)).toEqual(['edge:acc1:pve2>replica:acc1:pve2:pinned', 'path:vmbr0>guest:qemu/110|10.0.0.30:3000', 'replica:acc1:pve2:pinned>path:vmbr0', 'zone:example.com>edge:acc1:pve2'])
    expect(node('edge:acc1').routes).not.toContain(key)
  })

  test('a route that fails on every replica keeps its port red; the lines of the replicas stay plain', () => {
    expect(edge('path:vmbr0>guest:qemu/105#unreachable').style).toBe('unreachable')
    expect(edge('replica:acc1:pve1>path:vmbr0').style).toBe('plain')
  })

  test('the ids of the replicas read back', () => {
    expect(replicaNodeId('acc1', 'pve2')).toBe('replica:acc1:pve2')
    expect(replicaNodeId('acc1', 'pve2', true)).toBe('replica:acc1:pve2:pinned')
    expect(pinnedEdgeId('acc1', 'pve2')).toBe('edge:acc1:pve2')
    expect(replicaOf('replica:acc1:pve2')).toEqual({ account: 'acc1', node: 'pve2', pinned: false })
    expect(replicaOf('replica:acc1:pve2:pinned')).toEqual({ account: 'acc1', node: 'pve2', pinned: true })
    expect(replicaOf('connector:acc1')).toBeUndefined()
    // The account is up to the first separator, the node the rest, and a
    // pinned replica ends in its marker: a node named with the separator,
    // or named pinned, reads back as it is.
    expect(replicaOf(replicaNodeId('acc1', 'pve:2', true))).toEqual({ account: 'acc1', node: 'pve:2', pinned: true })
    expect(replicaOf(replicaNodeId('acc1', 'pve:2'))).toEqual({ account: 'acc1', node: 'pve:2', pinned: false })
    expect(replicaOf(replicaNodeId('acc1', 'pinned'))).toEqual({ account: 'acc1', node: 'pinned', pinned: false })
    expect(replicaOf('replica:acc1')).toBeUndefined()
    const tunnels = [
      { accountId: 'acc1', kind: 'shared', name: 'shared' },
      { accountId: 'acc1', kind: 'pinned', node: 'pve:2', name: 'pinned' },
    ] as TunnelView[]
    expect(tunnelOfEdge({ tunnels }, pinnedEdgeId('acc1', 'pve:2'))?.name).toBe('pinned')
    expect(tunnelOfEdge({ tunnels }, tunnelNodeId('acc1'))?.name).toBe('shared')
    expect(replicaState(undefined)).toBe('unknown')
    expect(replicaState({ state: 'active', replicaReady: false } as NodeView)).toBe('not ready')
    expect(replicaState({ state: 'down', replicaReady: false } as NodeView)).toBe('down')
  })

  test('the names of the cards', () => {
    expect(nodeLabel(node('replica:acc1:pve2'))).toBe('Replica pve2 of tunnel Main · 00000000: ready, 10 verified · 1 failing · 1 pinned')
    expect(nodeLabel(node('replica:acc1:pve3'))).toBe('Replica pve3 of tunnel Main · 00000000: joining, 0 verified')
    expect(nodeLabel(node('edge:acc1:pve2'))).toBe('Edge of tunnel pinned to pve2 · Main, verified: yes, connector on pve2')
  })
})

describe('the chains of a cluster', () => {
  const list = buildChains(st, tv)
  const steps = (hostname: string, owner: string) => {
    const c = list.find((x) => x.hostname === hostname && x.owner === owner)
    if (!c) throw new Error(`no chain ${hostname} ${owner}`)
    return c.steps.map((s) => `${s.name} ${s.level}: ${s.text}`)
  }

  test('a shared route: its placement and every replica, port (attested) with the node that vouched', () => {
    expect(steps('www.example.com', 'qemu/101')).toEqual([
      'Zone ok: example.com · served · Main',
      'Edge ok: Main · 00000000 · verified: yes',
      'Placement ok: shared tunnel pco-abc123',
      'Replicas ok: pve1: port · pve2: port (attested by pve1)',
      'Path ok: vmbr0 · VLAN 20 · direct',
      'Target ok: qemu/101 web-1 · 10.0.0.11:8080 http · level port',
    ])
  })

  test('a pinned route, one moving back, and one that fails everywhere', () => {
    expect(steps('git.example.com', 'qemu/110').slice(1, 4)).toEqual([
      'Edge ok: pinned to pve2 · Main · verified: yes',
      'Placement warn: pinned to pve2 (tunnel pco-abc123-pve2): pve1 does not reach it while pve2 does',
      'Replicas ok: pve2: port',
    ])
    expect(steps('grafana.example.com', 'lxc/205')[2]).toBe(
      'Placement warn: shared tunnel pco-abc123 · moving from pco-abc123-pve2 to pco-abc123: its connectors run the configuration with them',
    )
    expect(steps('grafana.example.com', 'lxc/205')[3]).toBe('Replicas ok: pve1: port (uplink) · pve2: port')
    expect(steps('monitor.example.com', 'qemu/105')[3]).toBe('Replicas fail: pve1: not reached (target is not answering) · pve2: not reached (no answer on port 8443 from 10.10.0.2)')
  })
})

// A cluster of n nodes, every route shared over all of them.
function wideCluster(n: number): State {
  const nodes: NodeView[] = Array.from({ length: n }, (_, i) => ({
    name: `pve${i + 1}`,
    state: 'active',
    kind: 'host',
    controller: i === 0,
    statusAge: 1e9,
    watchRunning: true,
    covered: true,
    replicaReady: i !== n - 1,
    verified: 4,
    failing: 0,
    belowMinimum: 0,
    pinned: 0,
    clockSkew: 0,
  }))
  const routes = st.routes.map((r) => (r.placement?.kind === 'shared' ? { ...r, replicas: nodes.map((x) => ({ node: x.name, level: 'port', ok: true, inSet: true })) } : r))
  return { ...st, nodes, routes }
}

describe('the replicas of a tunnel fold above six', () => {
  test('seven fold into one card that counts the ready ones, with one trunk and one line to each path', () => {
    const wide = wideCluster(7)
    const m = buildModel(wide, trafficFor(wide))
    expect(m.nodes.filter((n) => n.kind === 'replica' && !n.id.endsWith(':pinned'))).toHaveLength(7)
    const folded = foldReplicas(m, new Set())
    const group = folded.nodes.find((n) => n.id === replicasId('edge:acc1'))
    expect(group).toMatchObject({ kind: 'group', band: 'connector', label: 'replicas 6/7 ready', state: 'not ready', ref: 'edge:acc1', lines: ['Main · 00000000'] })
    expect(folded.nodes.filter((n) => n.kind === 'replica').map((n) => n.id)).toEqual(['replica:acc1:pve2:pinned'])
    const trunk = folded.edges.find((e) => e.id === `edge:acc1>${replicasId('edge:acc1')}`)
    expect(trunk).toMatchObject({ style: 'trunk', lanes: 4 })
    expect(trunk?.rate).toBeGreaterThan(0)
    const toPath = folded.edges.find((e) => e.id === `${replicasId('edge:acc1')}>path:vmbr0.20`)
    expect(toPath?.style).toBe('plain')
    expect(toPath?.routes).toContain(routeKey({ hostname: 'www.example.com', owner: 'qemu/101' }))
    const ids = new Set(folded.nodes.map((n) => n.id))
    expect(folded.edges.filter((e) => !ids.has(e.from) || !ids.has(e.to))).toEqual([])
    expect(nodeLabel(group as FlowNode)).toBe('replicas 6/7 ready of tunnel Main · 00000000, opens them')
  })

  test('six do not fold; an opened group and Expand all show every replica', () => {
    const six = wideCluster(foldedReplicas)
    const m6 = buildModel(six, trafficFor(six))
    expect(foldReplicas(m6, new Set())).toBe(m6)
    const wide = wideCluster(9)
    const m = buildModel(wide, trafficFor(wide))
    expect(collapse(m, { expanded: new Set() }).model.nodes.filter((n) => n.kind === 'replica')).toHaveLength(1)
    expect(collapse(m, { expanded: new Set([replicasId('edge:acc1')]) }).model.nodes.filter((n) => n.kind === 'replica')).toHaveLength(10)
    expect(collapse(m, { expanded: new Set([expandAll]) }).model.nodes.filter((n) => n.kind === 'replica')).toHaveLength(10)
  })

  test('a focus on a tunnel keeps its replicas, the one of a tunnel pinned to a node aside', () => {
    const routes = focusRoutes(model, 'tunnel:acc1')
    expect(routes.size).toBeGreaterThan(0)
    const view = collapse(model, { expanded: new Set(), focus: 'tunnel:acc1' }).model
    expect(view.nodes.filter((n) => n.kind === 'replica').map((n) => n.id)).toEqual(['replica:acc1:pve1', 'replica:acc1:pve2', 'replica:acc1:pve3'])
  })
})

describe('the map, the drawer and the extras of a cluster', () => {
  const view = collapse(model, { expanded: new Set() }).model

  test('a replica is a chip in the colour of its readiness, and says what it is', () => {
    const motion = createMotion({ cap: dotCap, frame: () => 1, cancel: () => undefined, hidden: () => false, reducedMotion: () => false })
    render(
      <FlowMap model={view} layout={layout(view)} motion={motion} onSelect={() => undefined} onFocus={() => undefined} onExpand={() => undefined} onViewport={() => undefined} reducedMotion={false} />,
    )
    const card = document.querySelector<HTMLElement>('[data-node="replica:acc1:pve2"]')
    expect(card?.classList.contains('fm-replica')).toBe(true)
    expect(card?.querySelector('.fm-chip')?.textContent).toBe('pve2')
    expect(card?.querySelector('.fm-chip')?.classList.contains('status-ok')).toBe(true)
    expect(card?.getAttribute('aria-label')).toBe('Replica pve2 of tunnel Main · 00000000: ready, 10 verified · 1 failing · 1 pinned')
    expect(document.querySelector<HTMLElement>('[data-node="replica:acc1:pve3"] .fm-chip')?.classList.contains('status-warn')).toBe(true)
    expect(document.querySelector('[data-node="edge:acc1:pve2"]')?.textContent).toContain('pinned to pve2')
  })

  test('a replica opens its node in the drawer', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ error: 'no target', code: 'not_found' }), { status: 404 })),
    )
    const { store } = await fakeStore({ state: st })
    render(
      <StoreProvider store={store}>
        <ToastProvider>
          <FlowDrawer model={view} selected="replica:acc1:pve2" onClose={() => undefined} />
        </ToastProvider>
      </StoreProvider>,
    )
    await flush()
    const drawer = screen.getByRole('complementary')
    expect(within(drawer).getAllByRole('heading')[0]?.textContent).toBe('pve2')
    await within(drawer).findByRole('list', { name: 'Routes pinned to it' })
    expect(drawer.textContent).toContain('heard 4s ago, watching')
    store.stop()
  })

  // The card of a pinned tunnel on the map leads with the node; the tunnel's
  // own card, and the pages, lead with the account.
  test.each([
    ['edge:acc1:pve2', 'pinned to pve2 · Main', 'Main · pinned to pve2'],
    ['edge:acc1:pve2>replica:acc1:pve2:pinned', 'Main · pinned to pve2', 'Main · pinned to pve2'],
    ['edge:acc1', 'Main · 00000000', 'Main · 00000000'],
  ])('%s opens its own tunnel, never the shared one of the account', async (selected, heading, card) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ error: 'no target', code: 'not_found' }), { status: 404 })),
    )
    const { store } = await fakeStore({ state: st })
    render(
      <StoreProvider store={store}>
        <ToastProvider>
          <FlowDrawer model={view} selected={selected} onClose={() => undefined} />
        </ToastProvider>
      </StoreProvider>,
    )
    await flush()
    const drawer = screen.getByRole('complementary')
    expect(within(drawer).getAllByRole('heading')[0]?.textContent).toBe(heading)
    if (selected.includes('>')) {
      const pinned = heading.includes('pinned')
      expect(within(drawer).getByRole('link', { name: 'The tunnel' }).getAttribute('href')).toBe(pinned ? '/edge/tunnels/acc1?tunnel=pco-abc123-pve2' : '/edge/tunnels/acc1')
    } else {
      expect(within(drawer).getByRole('region', { name: card })).toBeTruthy()
    }
    store.stop()
  })

  test('the rotate command sits on this node’s replica alone when its token is refused', () => {
    const refused: State = { ...st, connectors: st.connectors.map((c) => ({ ...c, tokenRefused: true })) }
    const extras = extrasOf(refused, tv as TrafficView, view)
    expect([...(extras.commands?.keys() ?? [])].sort()).toEqual(['replica:acc1:pve1', 'rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807'])
    expect(extras.concurrent?.has('edge:acc1>replica:acc1:pve1')).toBe(true)
    expect(extras.concurrent?.has('edge:acc1>replica:acc1:pve2')).toBe(false)
  })

  test('a focus on a replica follows the events of its account', () => {
    expect(eventsOf('replica:acc1:pve2')).toEqual({ account: ['acc1'] })
    expect(eventsOf('replica:acc1:pve:2:pinned')).toEqual({ account: ['acc1'] })
    expect(eventsOf('edge:acc1:pve2')).toEqual({ account: ['acc1'] })
    expect(eventsOf('replicas:edge:acc1')).toEqual({})
  })
})

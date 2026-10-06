import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import type { State, TrafficView } from '../api/types.gen'
import { ToastProvider } from '../components/Toast'
import { fakeStore, flush } from '../test/store'
import { collapse } from './collapse'
import { FlowDrawer } from './FlowDrawer'
import { buildModel } from './model'
import chains from './testdata/chains.json'
import { outage, trafficFor } from './testdata/scale'
import traffic from './testdata/traffic.json'
import type { Model } from './types'

const st = chains as unknown as State
const tv = traffic as unknown as TrafficView
const view = collapse(buildModel(st, tv), { expanded: new Set() }).model

afterEach(() => {
  vi.unstubAllGlobals()
})

async function open(selected: string, model: Model = view, state: State = st, trafficView: TrafficView = tv) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify({ error: 'no target', code: 'not_found' }), { status: 404 })),
  )
  const { store } = await fakeStore({ state, answers: { 'GET /api/v1/traffic': () => ({ status: 200, body: trafficView }) } })
  await flush()
  const onClose = vi.fn()
  render(
    <StoreProvider store={store}>
      <ToastProvider>
        <FlowDrawer model={model} selected={selected} onClose={onClose} />
      </ToastProvider>
    </StoreProvider>,
  )
  await flush()
  const drawer = screen.getByRole('complementary')
  return { store, onClose, drawer, title: within(drawer).getAllByRole('heading')[0]?.textContent }
}

describe('the drawer of the map', () => {
  test('a line of a zone card: the route', async () => {
    const { store, title } = await open('route:www.example.com qemu/101')
    expect(title).toBe('www.example.com')
    expect(screen.getByRole('tab', { name: 'Overview' })).toBeTruthy()
    expect(document.querySelector('.route-detail-drawer')).not.toBeNull()
    store.stop()
  })

  test('a hostname that waits for approval: its guest', async () => {
    const { store, title, drawer } = await open('route:dns.example.com lxc/202')
    expect(title).toBe('dns.example.com')
    expect(document.querySelector('.route-detail')).toBeNull()
    expect(drawer.textContent).toContain('lxc/202')
    store.stop()
  })

  test('a zone card: the zone', async () => {
    const { store, drawer, title } = await open('zone:example.com')
    expect(title).toBe('example.com')
    expect(document.querySelector('[id^="zone-"]')?.textContent).toBe('example.com')
    expect(within(drawer).getByRole('button', { name: 'Pin …' })).toBeTruthy()
    store.stop()
  })

  test.each(['edge:acc1', 'connector:acc1'])('%s: the tunnel', async (id) => {
    const { store, title, drawer } = await open(id)
    expect(title).toBe(id === 'edge:acc1' ? 'Main · 00000000' : 'pve1')
    expect(within(drawer).getByRole('heading', { name: 'Connector' })).toBeTruthy()
    expect(drawer.textContent).toContain('Configuration')
    store.stop()
  })

  test('a connector pco does not run: its fields and the rotate command, never a button that runs it', async () => {
    const { store, drawer } = await open('rogue:0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807')
    const term = (t: string) => within(drawer).getByText(t, { selector: 'dt' }).nextElementSibling?.textContent
    expect(term('Connects from')).toBe('198.51.100.7')
    expect(term('cloudflared')).toBe('2026.8.0')
    expect(term('Tunnel')).toBe('Main · 00000000')
    // acc1 is not an account id of Cloudflare's form: no command, and why
    expect(drawer.textContent).toContain('No command to copy: the account id has an unexpected form')
    expect(within(drawer).queryByRole('button', { name: /rotate/i })).toBeNull()
    store.stop()
  })

  test('a guest card: the guest', async () => {
    const { store, title } = await open('guest:qemu/101')
    expect(title).toBe('web-1')
    expect(document.querySelector('.route-detail')).toBeNull()
    store.stop()
  })

  test('a manual route’s address: its route', async () => {
    const { store, title } = await open('address:10.0.9.5')
    expect(title).toBe('intranet.example.com')
    expect(document.querySelector('.route-detail-drawer')).not.toBeNull()
    store.stop()
  })

  test('a path, and the hostnames in no zone: their routes in the chain list', async () => {
    const a = await open('path:vmbr0')
    expect(within(a.drawer).getByRole('list', { name: 'Their routes' }).querySelectorAll('.chain')).toHaveLength(1)
    a.store.stop()
  })

  test('the trunk: the requests and errors of the tunnel', async () => {
    const { store, title, drawer } = await open('edge:acc1>connector:acc1')
    expect(title).toBe('Main · 00000000')
    expect(within(drawer).getByRole('img', { name: 'Requests and errors' })).toBeTruthy()
    expect(drawer.textContent).toContain('requests in flight at the last sample')
    store.stop()
  })

  test('a line to a target: the connections opened to it', async () => {
    const { store, title, drawer } = await open('path:vmbr0.20>guest:qemu/101|10.0.0.11:8080')
    expect(title).toBe('10.0.0.11:8080')
    await flush()
    expect(drawer.textContent).toContain('The egress filter counts no target for this route yet.')
    store.stop()
  })

  test('a group of problems: its routes in the chain list, not on the map', async () => {
    const s = outage()
    const t = trafficFor(s)
    const grouped = collapse(buildModel(s, t), { expanded: new Set() }).model
    const row = grouped.nodes.flatMap((n) => n.rows ?? []).find((r) => r.kind === 'group')
    if (!row?.id) throw new Error('no group in the outage')
    const { store, drawer } = await open(row.id, grouped, s, t)
    expect(within(drawer).getByRole('list', { name: 'Their routes' }).querySelectorAll('.chain').length).toBeGreaterThan(0)
    store.stop()
  })

  test('Esc closes it', async () => {
    const { store, onClose, drawer } = await open('zone:example.com')
    fireEvent.keyDown(drawer, { key: 'Escape' })
    expect(onClose).toHaveBeenCalled()
    store.stop()
  })

  test('nothing picked, or what is gone: no drawer', async () => {
    const { store } = await fakeStore({ state: st })
    render(
      <StoreProvider store={store}>
        <FlowDrawer model={view} selected="guest:qemu/999" onClose={() => undefined} />
      </StoreProvider>,
    )
    expect(screen.queryByRole('complementary')).toBeNull()
    store.stop()
  })
})

import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { type AppStore, StoreProvider } from '../../api/store'
import type { Event, RouteSeries, State } from '../../api/types.gen'
import { ToastProvider } from '../../components/Toast'
import claims from '../../fixtures/claims.json'
import cluster from '../../fixtures/cluster.json'
import populated from '../../fixtures/populated.json'
import scenario from '../../fixtures/scenario-populated.json'
import { fakeStore, flush } from '../../test/store'
import { RouteDetail } from './RouteDetail'

type Handler = (url: string, init?: RequestInit) => Response

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
const notFound = () => json({ error: 'no target', code: 'not_found' }, 404)

function stubFetch(handle: Handler = notFound) {
  const fetch = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async (url, init) => handle(url, init))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

function show(store: AppStore, hostname: string, owner?: string, variant: 'drawer' | 'page' = 'drawer') {
  return render(
    <StoreProvider store={store}>
      <ToastProvider>
        <RouteDetail hostname={hostname} owner={owner} variant={variant} />
      </ToastProvider>
    </StoreProvider>,
  )
}

const detail = (term: string) => screen.queryByText(term, { selector: 'dt' })?.nextElementSibling

const series = (shared: number): RouteSeries => ({
  hostname: 'store.example.com',
  target: '10.0.0.40:80',
  shared,
  samples: Array.from({ length: 12 }, (_, i) => ({ at: new Date(Date.parse('2026-10-01T12:00:05Z') - (11 - i) * 5000).toISOString(), flowsPerSec: 1 + i / 10 })),
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the overview of a route', () => {
  test('the path with its VLAN, and the last proof stored', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: populated })
    show(store, 'www.example.com', 'qemu/101')
    expect(detail('Path')?.textContent).toBe('pve1 · vmbr0 · VLAN 20 · tap101i0')
    expect(detail('Last proof stored')?.querySelector('time')?.getAttribute('datetime')).toBe('2026-10-01T12:00:00Z')
    expect(detail('Last proof stored')?.textContent).toContain('pco stores a proof only when it changed')
    expect(detail('Bound since')?.querySelector('time')?.getAttribute('datetime')).toBe('2026-09-30T08:00:00Z')
  })

  test('a route reached through the leg of an attached network says how, and a direct one does not', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: populated })
    const { unmount } = show(store, 'zabbix.example.com', 'qemu/107')
    expect(detail('Path')?.textContent).toBe('pve1 · vmbr2 · tap107i0')
    expect(detail('Via')?.textContent).toBe('attach vmbr2: leg up, service 198.18.1.5 → 10.50.0.10')
    expect(detail('Service')?.textContent).toBe('http://198.18.1.5:8080')
    unmount()
    show(store, 'www.example.com', 'qemu/101')
    expect(detail('Via')).toBeUndefined()
  })

  test('no bridge proven for a route without a binding, as a manual route, and no proof to show', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: scenario })
    show(store, 'status.example.com', 'manual/status')
    expect(detail('Path')?.textContent).toBe('no bridge proven')
    expect(detail('Last proof stored')).toBeUndefined()
    expect(detail('Level')?.textContent).toBe('manual')
  })

  test('the rule, the tunnel by its account, the candidates', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: populated })
    show(store, 'www.example.com', 'qemu/101')
    expect(detail('Planned rule')?.textContent).toBe('servicehttp://10.0.0.11:8080httpHostHeaderintranet')
    const tunnel = within(detail('Tunnel') as HTMLElement).getByRole('link')
    expect(tunnel.textContent).toBe('account Main · 00000000')
    expect(tunnel.getAttribute('href')).toBe('/edge/tunnels/acc1')
    expect(detail('Candidates')?.textContent).toBe('10.0.0.11 static ok port')
    expect(within(detail('Owner') as HTMLElement).getByRole('link').getAttribute('href')).toBe('/guests/qemu/101')
  })

  test("the start of the tunnel's id is Cloudflare's text, shown as such", async () => {
    stubFetch()
    const st = populated as unknown as State
    const { store } = await fakeStore({ state: { ...st, tunnels: st.tunnels.map((t) => ({ ...t, id: 'ab\u202ecdef0-0000' })) } })
    show(store, 'www.example.com', 'qemu/101')
    const tunnel = within(detail('Tunnel') as HTMLElement).getByRole('link')
    expect(tunnel.textContent).toBe('account Main · ab⟨U+202E⟩cdef0')
  })

  test('a link opens exact names only', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: scenario })
    const { unmount } = show(store, 'www.example.com', 'qemu/101')
    const open = screen.getByRole('link', { name: 'Open https://www.example.com/' })
    expect(open.getAttribute('href')).toBe('https://www.example.com/')
    expect(open.getAttribute('rel')).toBe('noopener noreferrer')
    unmount()
    show(store, '*.store.example.com', 'lxc/210')
    expect(screen.queryByRole('link', { name: /^Open/ })).toBeNull()
    expect(screen.getByText('A wildcard also catches every name of the zone that has no record of its own.')).toBeTruthy()
  })

  test('a rejected route offers an admin to add its hostname to allowHosts', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: scenario })
    show(store, '*.store.example.com', 'lxc/210')
    expect(within(detail('Reason') as HTMLElement).getByRole('link', { name: 'Add to allowHosts' }).getAttribute('href')).toBe(
      '/settings?addAllowHost=*.store.example.com&owner=lxc%2F210',
    )
  })

  test('without an owner, the route that holds the hostname', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: populated })
    show(store, 'www.example.com', undefined, 'page')
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('www.example.com')
    expect(screen.getByText('Also asked for by', { exact: false }).textContent).toBe('Also asked for by qemu/102 (conflict).')
    expect(detail('Owner')?.textContent).toBe('web-1 qemu/101')
  })

  test('a route that is gone says so', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: populated })
    show(store, 'www.example.com', 'qemu/999')
    expect(screen.getByText('This route is gone')).toBeTruthy()
    expect(screen.getByText(/asks for/).textContent).toBe('qemu/999 asks for www.example.com no longer.')
    expect(screen.getByRole('link', { name: 'web-2 qemu/102' }).getAttribute('href')).toBe('/routes/www.example.com?owner=qemu%2F102')
  })
})

describe('a route on a cluster', () => {
  const clustered = cluster as unknown as State

  test('a shared route: its tunnel, every replica, and port (attested) with the node that vouched', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: clustered })
    show(store, 'www.example.com', 'qemu/101')
    expect(detail('Placement')?.textContent).toBe('shared tunnel pco-abc123')
    const replicas = screen.getByRole('list', { name: 'Replicas' })
    const items = within(replicas).getAllByRole('listitem').map((li) => li.textContent)
    expect(items).toEqual(['pve1portreached, in its set', 'pve2port (attested by pve1)reached, in its set'])
    expect(within(replicas).getByRole('link', { name: 'pve2' }).getAttribute('href')).toBe('/nodes/pve2')
    expect(detail('Move')).toBeFalsy()
  })

  test('a pinned route: its node, why, and the pinned tunnel it is served by', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: clustered })
    show(store, 'git.example.com', 'qemu/110')
    const placement = detail('Placement')
    expect(placement?.textContent).toBe('pinned to pve2 (tunnel pco-abc123-pve2)pve1 does not reach it while pve2 does')
    expect(within(placement as HTMLElement).getByRole('link', { name: 'pve2' }).getAttribute('href')).toBe('/nodes/pve2')
    const tunnel = within(detail('Tunnel') as HTMLElement).getByRole('link')
    expect(tunnel.textContent).toBe('account Main · pinned to pve2')
    expect(tunnel.getAttribute('href')).toBe('/edge/tunnels/acc1?tunnel=pco-abc123-pve2')
  })

  test('a move between tunnels in progress, and since when', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: clustered })
    show(store, 'grafana.example.com', 'lxc/205')
    expect(detail('Move')?.textContent).toContain('moving from pco-abc123-pve2 to pco-abc123: its connectors run the configuration with them')
    expect(detail('Move')?.querySelector('time')?.getAttribute('datetime')).toBe('2026-10-01T11:58:00Z')
  })

  test('the controller that proves the port of a guest of another node says it saw it behind its uplink', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: clustered })
    show(store, 'grafana.example.com', 'lxc/205')
    const items = within(screen.getByRole('list', { name: 'Replicas' }))
      .getAllByRole('listitem')
      .map((li) => li.textContent)
    expect(items).toEqual(['pve1port (uplink)reached, in its set', 'pve2portreached, in its set'])
  })

  test('a failing replica says why, and a route seen through an uplink is not shown as the guest’s own port', async () => {
    stubFetch()
    const st = structuredClone(clustered)
    const www = st.routes.find((r) => r.hostname === 'www.example.com' && r.owner === 'qemu/101')
    if (!www?.path) throw new Error('no route')
    www.path.port = 'enp3s0'
    www.guestNode = 'pve2'
    www.replicas = [
      { node: 'pve1', level: 'port', ok: true, inSet: true },
      { node: 'pve2', ok: false, inSet: false, reason: 'no answer on port 8080' },
    ]
    const { store } = await fakeStore({ state: st })
    show(store, 'www.example.com', 'qemu/101')
    expect(detail('Path')?.textContent).toBe('pve1 · vmbr0 · VLAN 20 · enp3s0The guest runs on pve2: enp3s0 is where pve1 sees it, not the port of the guest.')
    const items = within(screen.getByRole('list', { name: 'Replicas' }))
      .getAllByRole('listitem')
      .map((li) => li.textContent)
    expect(items).toEqual(['pve1port (uplink)reached, in its set', 'pve2-not reachedno answer on port 8080'])
  })

  test('a route of a node alone shows no placement and no replicas', async () => {
    stubFetch()
    const { store } = await fakeStore({ state: populated })
    show(store, 'www.example.com', 'qemu/101')
    expect(detail('Placement')).toBeFalsy()
    expect(screen.queryByRole('list', { name: 'Replicas' })).toBeNull()
  })
})

describe('the traffic of its target', () => {
  test('a shared target: one figure for every route on it, and the page says so', async () => {
    const fetch = stubFetch((url) => (url.startsWith('/api/v1/traffic/route') ? json(series(1)) : notFound()))
    const { store } = await fakeStore({ state: scenario })
    show(store, 'store.example.com', 'lxc/210')
    expect(await screen.findByText(/shared with 1 other route/)).toBeTruthy()
    expect(fetch.mock.calls.some(([url]) => url === '/api/v1/traffic/route?hostname=store.example.com')).toBe(true)
    expect(screen.getByRole('img', { name: 'Connections opened to the target' })).toBeTruthy()
    expect(screen.getByText('10.0.0.40:80')).toBeTruthy()
  })

  test('a target of its own: no word of sharing', async () => {
    stubFetch((url) => (url.startsWith('/api/v1/traffic/route') ? json(series(0)) : notFound()))
    const { store } = await fakeStore({ state: scenario })
    show(store, 'store.example.com', 'lxc/210')
    await screen.findByRole('img', { name: 'Connections opened to the target' })
    expect(screen.queryByText(/shared with/)).toBeNull()
  })

  test('without figures per route: why, in the words of the daemon, and nothing read', async () => {
    const fetch = stubFetch()
    const { store } = await fakeStore({ state: scenario })
    act(() => store.notice({ kind: 'traffic', data: { at: '2026-10-01T12:00:05Z', tunnels: [], routesTotal: 0, routesWhy: 'the egress filter is off' } }))
    show(store, 'store.example.com', 'lxc/210')
    expect(screen.getByText(/No figures per route/).textContent).toBe('No figures per route: the egress filter is off')
    await act(flush)
    expect(fetch.mock.calls.some(([url]) => url.startsWith('/api/v1/traffic/route'))).toBe(false)
  })
})

describe('the timeline and the claim', () => {
  test('the history of the route, read with the events log, and what the stream brought since, newest first', async () => {
    const history: Event[] = [
      { seq: 3, boot: 'old', at: '2026-09-30T08:00:00Z', level: 'info', kind: 'route', subject: 'www.example.com', message: 'www.example.com: active', route: 'www.example.com' },
      { seq: 4, boot: 'old', at: '2026-09-30T09:00:00Z', level: 'info', kind: 'admin', subject: 'qemu/101', message: 'handed over', route: 'www.example.com', actor: 'alice@pve (ticket)' },
    ]
    const fetch = stubFetch((url) => (url.startsWith('/api/v1/events') ? json(history) : notFound()))
    const { store } = await fakeStore({ state: populated })
    show(store, 'www.example.com', 'qemu/101')
    fireEvent.click(screen.getByRole('tab', { name: 'Timeline' }))
    const list = await screen.findByRole('list', { name: 'History of the route' })
    expect(fetch.mock.calls.some(([url]) => url === '/api/v1/events?route=www.example.com&history=1&limit=500')).toBe(true)
    const messages = within(list)
      .getAllByRole('listitem')
      .map((li) => li.querySelector('.timeline-message')?.textContent)
    // the event of the stream is the route's (events.json), the others of the log
    expect(messages).toEqual(['qemu/101: unreachable', 'handed over', 'www.example.com: active'])
    expect(within(list).getByText('alice@pve (ticket)').closest('.timeline-actor')?.textContent).toBe('by alice@pve (ticket)')
  })

  test('the claim, and handing it to the owner that waits', async () => {
    const fetch = stubFetch((url, init) => {
      if (url === '/api/v1/claims') return json(claims)
      if (url === '/api/v1/claims/resolve' && init?.method === 'POST') return json({})
      return notFound()
    })
    const { store } = await fakeStore({ state: populated })
    show(store, 'www.example.com', 'qemu/101')
    fireEvent.click(screen.getByRole('tab', { name: 'Claim' }))
    await screen.findByText('Holder', { selector: 'dt' })
    expect(detail('Holder')?.textContent).toBe('web-1 qemu/101')
    expect(detail('Waiting')?.textContent).toMatch(/^web-2 qemu\/102 since/)
    fireEvent.click(screen.getByRole('button', { name: 'Hand to …' }))
    const dialog = screen.getByRole('dialog')
    fireEvent.click(within(dialog).getByRole('radio', { name: /qemu\/102/ }))
    expect(within(dialog).getByText(/^Resolving hands/).textContent).toBe(
      'Resolving hands www.example.com to qemu/102 (web-2); qemu/101 (web-1) waits for it from then on, in the place in line its claim gives it.',
    )
    fireEvent.click(within(dialog).getByRole('button', { name: 'Hand it over' }))
    await screen.findByText(/is now held by/)
    const post = fetch.mock.calls.find(([url]) => url === '/api/v1/claims/resolve')
    expect(JSON.parse(String(post?.[1]?.body))).toEqual({ hostname: 'www.example.com', owner: 'qemu/102' })
  })

  test('a reader sees the claim, and why it cannot hand it over', async () => {
    const fetch = stubFetch((url) => (url === '/api/v1/claims' ? json(claims) : notFound()))
    const { store } = await fakeStore({ state: populated, session: { role: 'reader' } })
    show(store, 'www.example.com', 'qemu/101')
    fireEvent.click(screen.getByRole('tab', { name: 'Claim' }))
    const hand = await screen.findByRole('button', { name: 'Hand to …' })
    fireEvent.click(hand)
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.getByText('needs Sys.Modify on /')).toBeTruthy()
    expect(fetch.mock.calls.every(([url]) => url !== '/api/v1/claims/resolve')).toBe(true)
  })
})

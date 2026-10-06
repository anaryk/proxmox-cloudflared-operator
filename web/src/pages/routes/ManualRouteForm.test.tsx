import { act, fireEvent, render, screen, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { type AppStore, StoreProvider } from '../../api/store'
import type { ManualRouteView } from '../../api/types.gen'
import { navigate } from '../../app/router'
import { ToastProvider } from '../../components/Toast'
import guests from '../../fixtures/guests.json'
import routes from '../../fixtures/manual-routes.json'
import populated from '../../fixtures/populated.json'
import settings from '../../fixtures/settings.json'
import { fakeStore, flush } from '../../test/store'
import { ManualRoutePage } from './ManualRouteForm'

const stored = (routes as ManualRouteView[])[0] as ManualRouteView
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status })
const withCIDRs = { ...settings, settings: { ...settings.settings, manualCIDRs: ['10.0.5.0/24'] } }

interface Call {
  url: string
  method: string
  body?: unknown
}

function stubFetch(handle: (c: Call) => Response) {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const c = { url, method: init?.method ?? 'GET', body: init?.body ? JSON.parse(String(init.body)) : undefined }
      calls.push(c)
      return handle(c)
    }),
  )
  return calls
}

const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET')

async function show(store: AppStore, ui: ReactNode) {
  render(
    <StoreProvider store={store}>
      <ToastProvider>{ui}</ToastProvider>
    </StoreProvider>,
  )
  await act(flush)
}

const field = (name: string) => screen.getByRole('textbox', { name }) as HTMLInputElement
const type = (name: string, value: string) => fireEvent.change(field(name), { target: { value } })
const errorOf = (name: string) => {
  const id = field(name).getAttribute('aria-describedby') ?? ''
  return id
    .split(' ')
    .map((i) => document.getElementById(i))
    .find((el) => el?.classList.contains('field-error'))?.textContent
}

beforeEach(() => {
  navigate('/routes/manual/new', true)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('a new manual route', () => {
  test('the form says what is wrong at each field, and sends nothing', async () => {
    const calls = stubFetch(({ url }) => (url === '/api/v1/guests' ? json(guests) : json({}, 404)))
    const { store } = await fakeStore({ state: populated, answers: { 'GET /api/v1/settings': () => ({ status: 200, body: withCIDRs }) } })
    await show(store, <ManualRoutePage />)
    expect(screen.getByText('Allowed prefixes (manualCIDRs): 10.0.5.0/24.')).toBeTruthy()
    type('Id', 'Not An Id')
    type('Hostname', 'status')
    type('Address', '10.0.6.20')
    type('Port', '0')
    fireEvent.click(screen.getByRole('button', { name: 'Make the route' }))
    expect(errorOf('Id')).toBe('want 1 to 32 of a-z, 0-9 and -, or nothing for an id of its own')
    expect(errorOf('Hostname')).toBe('needs at least two labels')
    expect(errorOf('Address')).toBe('not inside the manualCIDRs of the settings (10.0.5.0/24)')
    expect(errorOf('Port')).toBe('want a port from 1 to 65535')
    expect(field('Hostname').getAttribute('aria-invalid')).toBe('true')
    expect(writes(calls)).toEqual([])
    // an error goes once the field is changed
    type('Hostname', 'status.example.com')
    expect(errorOf('Hostname')).toBeUndefined()
  })

  test('a guest target from the list of guests', async () => {
    const calls = stubFetch(({ url, method }) => {
      if (url === '/api/v1/guests') return json(guests)
      if (url === '/api/v1/routes/manual' && method === 'POST') return json({ ...stored, id: 'app', rev: 1, hostname: 'app.example.com' }, 201)
      return json([], 200)
    })
    const { store } = await fakeStore({ state: populated })
    await show(store, <ManualRoutePage />)
    fireEvent.click(screen.getByRole('radio', { name: /a guest/ }))
    const guest = screen.getByRole('combobox', { name: 'Guest' })
    expect(within(guest).getAllByRole('option').map((o) => o.textContent)).toEqual(['Choose a guest', 'qemu/101 web-1', 'lxc/200'])
    type('Hostname', 'app.example.com')
    fireEvent.change(guest, { target: { value: 'qemu/101' } })
    type('Port', '3000')
    fireEvent.click(screen.getByRole('button', { name: 'Make the route' }))
    await act(flush)
    expect(writes(calls)).toEqual([
      {
        url: '/api/v1/routes/manual',
        method: 'POST',
        body: { hostname: 'app.example.com', target: { kind: 'guest', scheme: 'http', guest: 'qemu/101', port: 3000 }, options: {} },
      },
    ])
    expect(window.location.pathname).toBe('/routes/manual/app')
  })

  test('a hostname another owner holds: the form says the route competes for it', async () => {
    stubFetch(() => json([]))
    const { store } = await fakeStore({ state: populated })
    await show(store, <ManualRoutePage />)
    type('Hostname', 'WWW.example.com')
    expect(screen.getByText(/is held by/).textContent).toBe(
      'www.example.com is held by qemu/101 (web-1) now: this route competes for it in the claims like any owner, and serves it only once it holds it.',
    )
  })

  test('an error of the daemon at a field goes to that field', async () => {
    stubFetch(({ url, method }) =>
      url === '/api/v1/routes/manual' && method === 'POST'
        ? json({ error: 'target.addr 10.0.5.1: an address of a node; a route to a service of the node needs allowNode', code: 'invalid', field: 'target.addr' }, 400)
        : json([]),
    )
    const { store } = await fakeStore({ state: populated, answers: { 'GET /api/v1/settings': () => ({ status: 200, body: withCIDRs }) } })
    await show(store, <ManualRoutePage />)
    type('Hostname', 'pve.example.com')
    type('Address', '10.0.5.1')
    type('Port', '8006')
    fireEvent.click(screen.getByRole('button', { name: 'Make the route' }))
    await act(flush)
    expect(errorOf('Address')).toBe('target.addr 10.0.5.1: an address of a node; a route to a service of the node needs allowNode')
  })

  test('allowNode asks a second time, naming the service of the node', async () => {
    const calls = stubFetch(({ url, method }) =>
      url === '/api/v1/routes/manual' && method === 'POST' ? json({ ...stored, id: 'pve', hostname: 'pve.example.com', options: { allowNode: true } }, 201) : json([]),
    )
    const { store } = await fakeStore({ state: populated, answers: { 'GET /api/v1/settings': () => ({ status: 200, body: withCIDRs }) } })
    await show(store, <ManualRoutePage />)
    type('Hostname', 'pve.example.com')
    type('Address', '10.0.5.1')
    type('Port', '8006')
    fireEvent.change(screen.getByRole('combobox', { name: 'Scheme' }), { target: { value: 'https' } })
    fireEvent.click(screen.getByRole('checkbox', { name: /allowNode/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Make the route' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText(/is a service of a node of the cluster/).textContent).toBe(
      'https://10.0.5.1:8006 is a service of a node of the cluster. Whoever reaches pve.example.com reaches that service on the node.',
    )
    expect(writes(calls)).toEqual([])
    fireEvent.click(within(dialog).getByRole('button', { name: 'Publish https://10.0.5.1:8006' }))
    await act(flush)
    expect(writes(calls).map((c) => c.body)).toEqual([
      { hostname: 'pve.example.com', target: { kind: 'address', scheme: 'https', addr: '10.0.5.1', port: 8006 }, options: { allowNode: true } },
    ])
  })
})

describe('a stored manual route', () => {
  test('a change carries the revision it was read at; refused, it is read again', async () => {
    let current = stored
    const calls = stubFetch(({ url, method }) => {
      if (url === '/api/v1/routes/manual' && method === 'GET') return json([current])
      if (url === '/api/v1/routes/manual/status' && method === 'PUT') {
        return json({ error: 'refused: the manual route manual/status changed since it was read at revision 2; read it again', code: 'refused' }, 409)
      }
      return json([])
    })
    const { store } = await fakeStore({ state: populated, answers: { 'GET /api/v1/settings': () => ({ status: 200, body: withCIDRs }) } })
    await show(store, <ManualRoutePage id="status" />)
    expect(field('Hostname').value).toBe('status.example.com')
    expect(screen.queryByRole('textbox', { name: 'Id' })).toBeNull()
    type('Port', '9001')
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await act(flush)
    expect(writes(calls)).toEqual([
      {
        url: '/api/v1/routes/manual/status',
        method: 'PUT',
        body: { rev: 2, hostname: 'status.example.com', target: { kind: 'address', scheme: 'http', addr: '10.0.5.20', port: 9001 }, options: {} },
      },
    ])
    expect(screen.getByRole('alert').textContent).toBe('refused: the manual route manual/status changed since it was read at revision 2; read it again')
    // another admin saved revision 3 meanwhile
    current = { ...stored, rev: 3, target: { ...stored.target, port: 9100 } }
    fireEvent.click(screen.getByRole('button', { name: 'Look again' }))
    await act(flush)
    expect(field('Port').value).toBe('9100')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  test('delete asks, and names the revision', async () => {
    const calls = stubFetch(({ url, method }) => {
      if (url === '/api/v1/routes/manual' && method === 'GET') return json([stored])
      if (method === 'DELETE') return json({})
      return json([])
    })
    const { store } = await fakeStore({ state: populated })
    await show(store, <ManualRoutePage id="status" />)
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
    const dialog = screen.getByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete the route' }))
    await act(flush)
    expect(writes(calls)).toEqual([{ url: '/api/v1/routes/manual/status?rev=2', method: 'DELETE', body: undefined }])
    expect(window.location.pathname).toBe('/routes')
  })

  test('a reader sees it, and changes nothing', async () => {
    stubFetch(({ url }) => (url === '/api/v1/routes/manual' ? json([stored]) : json([])))
    const { store } = await fakeStore({ state: populated, session: { role: 'reader' } })
    await show(store, <ManualRoutePage id="status" />)
    expect(field('Hostname').disabled).toBe(true)
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull()
    expect(screen.getByText('Only an admin changes manual routes: needs Sys.Modify on /.')).toBeTruthy()
  })

  test('one that is not there', async () => {
    stubFetch(() => json([]))
    const { store } = await fakeStore({ state: populated })
    await show(store, <ManualRoutePage id="gone" />)
    expect(screen.getByText('There is no such manual route')).toBeTruthy()
  })
})

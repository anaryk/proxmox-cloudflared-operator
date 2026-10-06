import { fireEvent, render, screen, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { type AppStore, StoreProvider } from '../../api/store'
import type { State } from '../../api/types.gen'
import { navigate } from '../../app/router'
import { HeldIcon, RejectedIcon } from '../../components/icons'
import { ToastProvider } from '../../components/Toast'
import empty from '../../fixtures/empty.json'
import populated from '../../fixtures/populated.json'
import scenario from '../../fixtures/scenario-populated.json'
import { fakeStore } from '../../test/store'
import { RoutesSection } from './RoutesPage'

function show(store: AppStore, ui: ReactNode) {
  return render(
    <StoreProvider store={store}>
      <ToastProvider>{ui}</ToastProvider>
    </StoreProvider>,
  )
}

const bodyRows = () => screen.getAllByRole('row').filter((r) => r.closest('tbody') && r.getAttribute('aria-hidden') !== 'true' && r.querySelector('td.lead'))
const hostnames = () => bodyRows().map((r) => r.querySelector('td.lead .cell-line')?.textContent?.replace(/wildcard$/, '').trim())
const ownersShown = () => bodyRows().map((r) => r.querySelector('td[data-label="Owner"] .mono')?.textContent)

beforeEach(() => {
  navigate('/routes', true)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the routes table', () => {
  test('sorted by hostname, then owner as model.CompareOwners', async () => {
    const { store } = await fakeStore({ state: scenario })
    show(store, <RoutesSection view={{ name: 'routes' }} />)
    expect(hostnames().slice(0, 3)).toEqual(['*.store.example.com', 'app.example.com', 'db-admin.example.com'])
    const www = bodyRows().filter((r) => r.querySelector('td.lead .cell-line')?.textContent === 'www.example.com')
    expect(www.map((r) => r.querySelector('td[data-label="Owner"] .mono')?.textContent)).toEqual(['qemu/101', 'qemu/102'])
    expect(hostnames()).toHaveLength(16)
  })

  test('the chips count every state in the order of pco status, and filter', async () => {
    const { store } = await fakeStore({ state: scenario })
    show(store, <RoutesSection view={{ name: 'routes' }} />)
    const chips = within(screen.getByRole('group', { name: 'States' })).getAllByRole('button')
    expect(chips.map((c) => c.textContent)).toEqual(['active 8', 'unreachable 1', 'withdrawn 1', 'conflict 1', 'no-zone 2', 'held 1', 'rejected 1', 'frozen 1'])
    fireEvent.click(chips[4] as HTMLElement)
    expect(chips[4]?.getAttribute('aria-pressed')).toBe('true')
    expect(hostnames()).toEqual(['media.example.io', 'wiki.example.org'])
    fireEvent.click(chips[6] as HTMLElement)
    expect(hostnames()).toEqual(['*.store.example.com', 'media.example.io', 'wiki.example.org'])
    expect(screen.getByText('3 of 16 routes')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Clear the filter' }))
    expect(hostnames()).toHaveLength(16)
  })

  test('the search looks at the hostname, the owner, the guest name and the service', async () => {
    const { store } = await fakeStore({ state: scenario })
    show(store, <RoutesSection view={{ name: 'routes' }} />)
    const search = screen.getByRole('searchbox', { name: 'Search the routes' })
    fireEvent.change(search, { target: { value: 'gitea' } })
    expect(hostnames()).toEqual(['git.example.com'])
    fireEvent.change(search, { target: { value: 'manual/' } })
    expect(hostnames()).toEqual(['status.example.com'])
    fireEvent.change(search, { target: { value: ':51821' } })
    expect(hostnames()).toEqual(['vpn.example.com'])
    fireEvent.change(search, { target: { value: 'nothing-like-it' } })
    expect(screen.getByText(/No route matches the filter/)).toBeTruthy()
  })

  test('the populated fixture of the engine: two owners of one hostname', async () => {
    const { store } = await fakeStore({ state: populated })
    show(store, <RoutesSection view={{ name: 'routes' }} />)
    expect(hostnames()).toEqual(['www.example.com', 'www.example.com'])
    expect(ownersShown()).toEqual(['qemu/101', 'qemu/102'])
    // the note under the hostname: the reason, else the first warning
    expect(bodyRows().map((r) => r.querySelector('.route-note')?.textContent)).toEqual(['a warning', 'hostname is held by qemu/101'])
    // a click selects the route in the address
    fireEvent.click(bodyRows()[1] as HTMLElement)
    expect(window.location.pathname + window.location.search).toBe('/routes/www.example.com?owner=qemu%2F102')
  })

  test('the sentence of the CLI over routes of an earlier cycle, and before the first', async () => {
    const held = await fakeStore({ state: populated })
    const { unmount } = show(held.store, <RoutesSection view={{ name: 'routes' }} />)
    expect(screen.getByText(/The last cycle held \(/).textContent).toBe('The last cycle held (no writer identity; run pco setup): these are the routes of an earlier cycle.')
    unmount()
    const first = await fakeStore({ state: { ...empty, digest: '1111111111111111' } })
    show(first.store, <RoutesSection view={{ name: 'routes' }} />)
    expect(screen.getAllByText('Waiting for the first cycle')).toHaveLength(1)
    expect(screen.queryByRole('table')).toBeNull()
  })
})

describe('a rejected route', () => {
  const rejectedRow = () => bodyRows().find((r) => r.querySelector('td.lead .cell-line')?.textContent?.startsWith('*.store.example.com')) as HTMLElement

  test('its chip, its icon and the reason that names the pattern', async () => {
    const { store } = await fakeStore({ state: scenario })
    show(store, <RoutesSection view={{ name: 'routes' }} />)
    const row = rejectedRow()
    const chip = row.querySelector('td[data-label="State"] .status') as HTMLElement
    expect(chip.textContent).toBe('rejected')
    const { container } = render(
      <>
        <RejectedIcon />
        <HeldIcon />
      </>,
    )
    const [rejected, held] = [...container.querySelectorAll('svg')]
    expect(chip.querySelector('svg')?.isEqualNode(rejected ?? null)).toBe(true)
    expect(chip.querySelector('svg')?.isEqualNode(held ?? null)).toBe(false)
    expect(within(row).getByText('wildcard')).toBeTruthy()
    expect(row.querySelector('.route-note bdi')?.textContent).toBe(
      'a wildcard is published only when an allowHosts pattern names it: add "*.store.example.com" to allowHosts',
    )
  })

  test('an admin may add its hostname to allowHosts, encoded with the owner; a reader may not', async () => {
    const admin = await fakeStore({ state: scenario })
    const { unmount } = show(admin.store, <RoutesSection view={{ name: 'routes' }} />)
    const links = screen.getAllByRole('link', { name: 'Add to allowHosts' })
    expect(links).toHaveLength(1)
    expect(links[0]?.getAttribute('href')).toBe('/settings?addAllowHost=*.store.example.com&owner=lxc%2F210')
    expect(rejectedRow().contains(links[0] as HTMLElement)).toBe(true)
    fireEvent.click(links[0] as HTMLElement)
    expect(window.location.pathname + window.location.search).toBe('/settings?addAllowHost=*.store.example.com&owner=lxc%2F210')
    unmount()
    const reader = await fakeStore({ state: scenario, session: { role: 'reader' } })
    show(reader.store, <RoutesSection view={{ name: 'routes' }} />)
    expect(screen.queryByRole('link', { name: 'Add to allowHosts' })).toBeNull()
  })

  test('the pattern is the hostname of the state, never a word of the reason', async () => {
    const st = scenario as unknown as State
    const odd = {
      ...st,
      routes: st.routes.map((r) => (r.state === 'rejected' ? { ...r, reason: 'add "evil.example.net" to allowHosts' } : r)),
    }
    const { store } = await fakeStore({ state: odd })
    show(store, <RoutesSection view={{ name: 'routes' }} />)
    expect(screen.getByRole('link', { name: 'Add to allowHosts' }).getAttribute('href')).toBe('/settings?addAllowHost=*.store.example.com&owner=lxc%2F210')
  })

  test('a manual route never offers it', async () => {
    const st = scenario as unknown as State
    const manual = { ...st, routes: st.routes.map((r) => (r.state === 'rejected' ? { ...r, owner: 'manual/odd' } : r)) }
    const { store } = await fakeStore({ state: manual })
    show(store, <RoutesSection view={{ name: 'routes' }} />)
    expect(screen.queryByRole('link', { name: 'Add to allowHosts' })).toBeNull()
  })
})

describe('the actions of the page', () => {
  test('Sync now asks for a cycle; a reader is told why not', async () => {
    const fetch = vi.fn(async () => new Response('{}', { status: 202 }))
    vi.stubGlobal('fetch', fetch)
    const admin = await fakeStore({ state: populated })
    const { unmount } = show(admin.store, <RoutesSection view={{ name: 'routes' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
    await screen.findByText('A cycle was requested.')
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(fetch.mock.calls[0]).toMatchObject(['/api/v1/sync', { method: 'POST' }])
    unmount()
    const reader = await fakeStore({ state: populated, session: { role: 'reader' } })
    show(reader.store, <RoutesSection view={{ name: 'routes' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Sync now' }))
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(screen.getAllByText('needs Sys.Modify on /')).toHaveLength(2)
  })

  test('a route opens beside the table, named by its hostname, and closes back to the list', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('[]', { status: 200 })))
    const { store } = await fakeStore({ state: populated })
    show(store, <RoutesSection view={{ name: 'route', hostname: 'www.example.com', owner: 'qemu/101' }} />)
    const drawer = screen.getByRole('complementary')
    expect(within(drawer).getByRole('heading', { level: 2 }).textContent).toBe('www.example.com')
    expect(bodyRows()[0]?.getAttribute('aria-current')).toBe('true')
    fireEvent.click(within(drawer).getByRole('button', { name: 'Close' }))
    expect(window.location.pathname).toBe('/routes')
  })
})

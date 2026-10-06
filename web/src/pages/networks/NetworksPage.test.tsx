import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import { AppStore, StoreProvider } from '../../api/store'
import type { RouteView, State } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'
import { fakeStore, memoryStorage } from '../../test/store'
import { NetworksPage } from './NetworksPage'

const base = populated as State
const manual: RouteView = { hostname: 'status.example.com', owner: 'manual/status', state: 'active', level: 'manual', service: 'http://10.0.5.20:9000', zone: 'example.com' }

// The populated state has a route of qemu/101 proven on vmbr0 with the tag 20,
// and one that lost its hostname; a manual route is added to it.
const state: State = { ...base, routes: [...base.routes, manual] }

async function mount(st: State = state) {
  const { store } = await fakeStore({ state: st })
  return render(
    <StoreProvider store={store}>
      <NetworksPage />
    </StoreProvider>,
  )
}

const table = (name: string) => screen.getByRole('table', { name })
const rowsOf = (name: string) =>
  within(table(name))
    .getAllByRole('row')
    .slice(1)
    .map((r) => within(r).getAllByRole('cell').map((c) => c.textContent))

describe('the Direct table', () => {
  test('has a row for each bridge and VLAN, and one for the routes proven on no bridge', async () => {
    await mount()
    expect(rowsOf('Direct networks')).toEqual([
      ['vmbr0', '20', '1', 'port 1', '1'],
      ['no bridge proven', '-', '1', 'manual 1', '0'],
    ])
  })

  test('has no subnet, and says why', async () => {
    await mount()
    expect(screen.queryByText(/\d+\.\d+\.\d+\.\d+\/\d+/)).toBeNull()
    expect(screen.getByText(/It shows no subnet/)).toBeTruthy()
    expect(within(table('Direct networks')).queryByRole('columnheader', { name: /subnet/i })).toBeNull()
  })

  test('counts the routes that are on no network', async () => {
    await mount()
    expect(screen.getByText(/1 route is in no row/)).toBeTruthy()
  })

  test('says why it is empty when no route was proven', async () => {
    await mount({ ...base, routes: base.routes.slice(1) })
    expect(screen.getByText(/No route has been proven on a bridge yet/)).toBeTruthy()
  })

  test('a row opens the routes and the guests of its network', async () => {
    await mount()
    fireEvent.click(within(table('Direct networks')).getByText('vmbr0'))
    const drawer = screen.getByRole('complementary')
    expect(within(drawer).getByRole('heading', { name: 'vmbr0, VLAN 20' })).toBeTruthy()
    expect(within(drawer).getByRole('link', { name: 'www.example.com' }).getAttribute('href')).toBe('/routes/www.example.com?owner=qemu%2F101')
    expect(within(drawer).getByText('tap101i0').parentElement?.textContent).toMatch(/^qemu\/101, port tap101i0, verified /)
    expect(within(drawer).getByRole('heading', { name: '1 guest' })).toBeTruthy()
    expect(within(drawer).getByText('web-1')).toBeTruthy()
    // as the owner of the route, and in the list of guests
    expect(within(drawer).getAllByText('qemu/101')).toHaveLength(2)
  })

  test('the row without a bridge says what is in it, and has no guest for a manual route', async () => {
    await mount()
    fireEvent.click(within(table('Direct networks')).getByText('no bridge proven'))
    const drawer = screen.getByRole('complementary')
    expect(within(drawer).getByRole('heading', { name: 'No bridge proven' })).toBeTruthy()
    expect(within(drawer).getByText(/manual routes, which name an address and no guest/)).toBeTruthy()
    expect(within(drawer).getByRole('link', { name: 'status.example.com' }).getAttribute('href')).toBe('/routes/status.example.com?owner=manual%2Fstatus')
    expect(within(drawer).getByText('None: no route here belongs to a guest.')).toBeTruthy()
  })

  test('shows the tag of a guest in an untagged part of a bridge as untagged', async () => {
    const untagged: RouteView = { ...(base.routes[0] as RouteView), hostname: 'plain.example.com', owner: 'qemu/103', path: { ...(base.routes[0]?.path as NonNullable<RouteView['path']>), vlan: undefined } }
    await mount({ ...base, routes: [untagged, ...base.routes] })
    expect(rowsOf('Direct networks').map((r) => r.slice(0, 3))).toEqual([
      ['vmbr0', 'untagged', '1'],
      ['vmbr0', '20', '1'],
    ])
  })
})

describe('the page', () => {
  test('explains Direct, the levels and identityMinimum', async () => {
    await mount()
    expect(screen.getByText(/With Direct, pco reaches a guest on the network the node itself is on/)).toBeTruthy()
    for (const level of ['port', 'observed', 'filtered', 'manual']) {
      expect(within(screen.getByRole('region', { name: 'How a route is proven' })).getByText(level, { selector: 'dt b' })).toBeTruthy()
    }
    const minimum = screen.getByText('identityMinimum', { selector: 'b' }).parentElement
    expect(minimum?.textContent).toContain('here it is port')
  })

  test('has the segments of routes at observed', async () => {
    await mount()
    expect(rowsOf('Segments').map((r) => [r[0], r[1], r[3]])).toEqual([
      ['vmbr1', 'untagged', '2'],
      ['vmbr1', '20', '1'],
    ])
    expect(rowsOf('Segments')[1]?.[2]).toBe('no')
  })

  test('has no segments section without segments', async () => {
    await mount({ ...base, segments: [] })
    expect(screen.queryByRole('region', { name: 'Segments of routes at observed' })).toBeNull()
  })

  test('is laid out in sections of their own, Direct first', async () => {
    await mount()
    const names = screen.getAllByRole('region').map((r) => r.getAttribute('aria-labelledby'))
    expect(names).toEqual(['networks-direct', 'networks-segments', 'networks-identity'])
  })

  test('offers nothing to attach or manage', async () => {
    await mount()
    expect(screen.queryByRole('button', { name: /attach|managed|consent/i })).toBeNull()
    expect(screen.queryByRole('heading', { name: /managed/i })).toBeNull()
  })

  test('holds the place of the networks until the state is there', () => {
    render(
      <StoreProvider store={new AppStore({ storages: { session: memoryStorage(), local: memoryStorage() } })}>
        <NetworksPage />
      </StoreProvider>,
    )
    expect(screen.getByRole('status').textContent).toContain('Loading the networks')
  })
})

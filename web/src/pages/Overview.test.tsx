import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import first from '../fixtures/first-run.json'
import populated from '../fixtures/populated.json'
import trafficFixture from '../fixtures/traffic.json'
import { navigate } from '../app/router'
import chains from '../flow/testdata/chains.json'
import traffic from '../flow/testdata/traffic.json'
import { fakeStore, flush } from '../test/store'
import { eventsOf, Overview } from './Overview'

async function show(state: unknown, trafficView?: unknown) {
  const answers = trafficView ? { 'GET /api/v1/traffic': () => ({ status: 200, body: trafficView }) } : undefined
  const { store } = await fakeStore({ state, answers })
  await flush()
  render(
    <StoreProvider store={store}>
      <Overview />
    </StoreProvider>,
  )
  return store
}

const hosts = () => [...document.querySelectorAll('.chain-host')].map((h) => h.textContent)

describe('the Overview', () => {
  test('the figures, the problems, every hostname and the live events', async () => {
    navigate('/')
    const store = await show(chains, traffic)
    expect(screen.getByRole('heading', { level: 1, name: 'Overview' })).toBeTruthy()
    expect(screen.getByRole('region', { name: 'Routes' })).toBeTruthy()
    expect(screen.getByRole('heading', { name: '1 problem' })).toBeTruthy()
    const flow = within(screen.getByRole('region', { name: 'Flow' }))
    expect(flow.getByRole('list', { name: 'Hostnames and their chains' })).toBeTruthy()
    expect(hosts()).toHaveLength(13)
    expect(screen.getByRole('region', { name: 'Live events' }).querySelector('table')).toBeTruthy()
    store.stop()
  })

  test('the focus in the address shows its chains only, and the search box writes it', async () => {
    navigate('/?focus=hostname%3Awww.example.com')
    const store = await show(chains, traffic)
    expect(hosts()).toEqual(['www.example.com', 'www.example.com'])
    const search = screen.getByRole('searchbox', { name: 'Focus a hostname, guest, zone or tunnel' }) as HTMLInputElement
    expect(search.value).toBe('hostname:www.example.com')
    act(() => {
      fireEvent.change(search, { target: { value: 'lab' } })
    })
    expect(new URLSearchParams(window.location.search).get('focus')).toBe('lab')
    expect(hosts()).toEqual(['lab.example.dev'])
    act(() => {
      fireEvent.change(search, { target: { value: '' } })
    })
    expect(window.location.search).toBe('')
    expect(hosts()).toHaveLength(13)
    store.stop()
  })

  test('problems first is in the address too', async () => {
    navigate('/')
    const store = await show(chains, traffic)
    const box = screen.getByRole('checkbox', { name: 'Problems first' }) as HTMLInputElement
    // a small map shows its hostnames by name
    expect(box.checked).toBe(false)
    act(() => {
      fireEvent.click(box)
    })
    expect(new URLSearchParams(window.location.search).get('problems')).toBe('1')
    expect(hosts().slice(0, 2)).toEqual(['api.example.com', 'dns.example.com'])
    store.stop()
  })

  test('before the first cycle: waiting, with the standing problems, not "no routes"', async () => {
    navigate('/')
    const store = await show(first)
    expect(within(screen.getByRole('region', { name: 'Flow' })).getByText('Waiting for the first cycle')).toBeTruthy()
    expect(screen.getByRole('region', { name: 'Routes' }).textContent).toContain('waiting for the first cycle')
    expect(screen.getByText('no Cloudflare credential; add one with pco credential add')).toBeTruthy()
    expect(screen.queryByText(/No guest carries/)).toBeNull()
    store.stop()
  })

  test('after a cycle without hostnames: why there are none', async () => {
    navigate('/')
    const store = await show({ ...populated, routes: [], unapproved: [], gateTagged: 0 }, trafficFixture)
    expect(within(screen.getByRole('region', { name: 'Flow' })).getByText(/No guest carries the tag/)).toBeTruthy()
    expect(screen.queryByRole('searchbox')).toBeNull()
    store.stop()
  })
})

test.each([
  [undefined, {}],
  ['hostname:www.example.com', { route: ['www.example.com'] }],
  ['route:www.example.com qemu/101', { route: ['www.example.com'] }],
  ['guest:qemu/101', { guest: ['qemu/101'] }],
  ['tunnel:acc1', { account: ['acc1'] }],
  ['connector:acc1', { account: ['acc1'] }],
  ['zone:example.com', { text: 'example.com' }],
  ['path:vmbr0', {}],
  ['web-1', { text: 'web-1' }],
  ['10.0.0.11:8080', { text: '10.0.0.11:8080' }],
])('the live events of the focus %s', (focus, filter) => {
  expect(eventsOf(focus)).toEqual(filter)
})

import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import first from '../fixtures/first-run.json'
import populated from '../fixtures/populated.json'
import trafficFixture from '../fixtures/traffic.json'
import { navigate } from '../app/router'
import chains from '../flow/testdata/chains.json'
import { scaled } from '../flow/testdata/scale'
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
  test('the figures, the problems, the map of every hostname and the live events', async () => {
    navigate('/')
    const store = await show(chains, traffic)
    expect(screen.getByRole('heading', { level: 1, name: 'Overview' })).toBeTruthy()
    expect(screen.getByRole('region', { name: 'Routes' })).toBeTruthy()
    expect(screen.getByRole('heading', { name: '1 problem' })).toBeTruthy()
    const flow = within(screen.getByRole('region', { name: 'Flow' }))
    const map = await flow.findByRole('group', { name: 'Flow map' })
    expect(map.querySelectorAll('.fm-row')).toHaveLength(13)
    expect(flow.getByRole('list', { name: 'Legend' })).toBeTruthy()
    expect(flow.queryByRole('list', { name: 'Hostnames and their chains' })).toBeNull()
    expect(screen.getByRole('region', { name: 'Live events' }).querySelector('table')).toBeTruthy()
    store.stop()
  })

  test('List view brings the chain list back, and the address keeps it', async () => {
    navigate('/')
    const store = await show(chains, traffic)
    const flow = within(screen.getByRole('region', { name: 'Flow' }))
    await flow.findByRole('group', { name: 'Flow map' })
    const toggle = flow.getByRole('button', { name: 'List view' })
    expect(toggle.getAttribute('aria-pressed')).toBe('false')
    act(() => {
      fireEvent.click(toggle)
    })
    expect(new URLSearchParams(window.location.search).get('list')).toBe('1')
    expect(flow.getByRole('list', { name: 'Hostnames and their chains' })).toBeTruthy()
    expect(flow.queryByRole('group', { name: 'Flow map' })).toBeNull()
    expect(hosts()).toHaveLength(13)
    expect(flow.queryByRole('button', { name: 'Pause motion' })).toBeNull()
    act(() => {
      fireEvent.click(flow.getByRole('button', { name: 'List view' }))
    })
    expect(window.location.search).toBe('')
    expect(await flow.findByRole('group', { name: 'Flow map' })).toBeTruthy()
    store.stop()
  })

  test('a phone gets the chain list only', async () => {
    navigate('/')
    const narrow = vi.spyOn(window, 'matchMedia').mockImplementation(
      (q: string) => ({ matches: q === '(max-width: 719px)', media: q, addEventListener: () => undefined, removeEventListener: () => undefined }) as unknown as MediaQueryList,
    )
    const store = await show(chains, traffic)
    const flow = within(screen.getByRole('region', { name: 'Flow' }))
    expect(flow.getByRole('list', { name: 'Hostnames and their chains' })).toBeTruthy()
    expect(flow.queryByRole('button', { name: 'List view' })).toBeNull()
    expect(flow.queryByRole('link', { name: 'Skip the map' })).toBeNull()
    narrow.mockRestore()
    store.stop()
  })

  test('a link skips the map; a node opens its drawer; motion pauses', async () => {
    navigate('/')
    const store = await show(chains, traffic)
    const flow = within(screen.getByRole('region', { name: 'Flow' }))
    const map = await flow.findByRole('group', { name: 'Flow map' })
    const skip = flow.getByRole('link', { name: 'Skip the map' })
    expect(skip.compareDocumentPosition(map) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    act(() => {
      fireEvent.click(skip)
    })
    expect(document.activeElement?.id).toBe('flow-end')
    expect(map.compareDocumentPosition(document.activeElement as Element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()

    const node = [...map.querySelectorAll<HTMLElement>('[data-item]')].find((el) => el.dataset.item === 'edge:acc1')
    act(() => {
      node?.click()
    })
    const drawer = screen.getByRole('complementary')
    expect(within(drawer).getAllByRole('heading')[0]?.textContent).toBe('Main · 00000000')

    const pause = flow.getByRole('button', { name: 'Pause motion' })
    act(() => {
      fireEvent.click(pause)
    })
    expect(flow.getByRole('button', { name: 'Resume motion' }).getAttribute('aria-pressed')).toBe('true')
    expect(flow.getByRole('list', { name: 'Legend' }).textContent).toContain('Motion is paused')
    store.stop()
  })

  test('Expand all opens every folded card, in the address', async () => {
    navigate('/')
    const store = await show(chains, traffic)
    const flow = within(screen.getByRole('region', { name: 'Flow' }))
    await flow.findByRole('group', { name: 'Flow map' })
    act(() => {
      fireEvent.click(flow.getByRole('button', { name: 'Expand all' }))
    })
    expect(new URLSearchParams(window.location.search).getAll('expand')).toEqual(['*'])
    expect(flow.getByRole('button', { name: 'Expand all' }).getAttribute('aria-pressed')).toBe('true')
    store.stop()
  })

  test('the focus in the address shows its chains only, and the search box writes it', async () => {
    navigate('/?list=1&focus=hostname%3Awww.example.com')
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
    expect(window.location.search).toBe('?list=1')
    expect(hosts()).toHaveLength(13)
    store.stop()
  })

  test('problems first is in the address too', async () => {
    navigate('/?list=1')
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

  test('near the limit of a collapsed map, problems first keeps its default from one state to the next', async () => {
    navigate('/')
    let current = { ...scaled({ routes: 201 }), digest: 'a1' }
    const { store } = await fakeStore({ state: current, answers: { 'GET /api/v1/state': () => ({ status: 200, body: current, etag: current.digest }) } })
    render(
      <StoreProvider store={store}>
        <Overview />
      </StoreProvider>,
    )
    const box = () => screen.getByRole('checkbox', { name: 'Problems first' }) as HTMLInputElement
    expect(box().checked).toBe(true)
    // 195 routes would be folded for a map seen for the first time
    current = { ...scaled({ routes: 195 }), digest: 'a2' }
    await act(async () => {
      store.notice({ kind: 'state', data: { at: '2026-10-01T12:00:10Z', finishedAt: '2026-10-01T12:00:12Z', digest: 'a2' } })
      await flush()
    })
    expect(store.get().state?.routes).toHaveLength(195)
    expect(box().checked).toBe(true)
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

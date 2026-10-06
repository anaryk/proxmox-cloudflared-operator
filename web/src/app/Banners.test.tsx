import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import type { State } from '../api/types.gen'
import populated from '../fixtures/populated.json'
import untagged from '../fixtures/untagged.json'
import { appState, fakeStore } from '../test/store'
import { Banners, bannersOf } from './Banners'

const now = Date.parse('2026-10-01T12:05:00Z')
const st = populated as unknown as State

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(now)
})

afterEach(() => {
  vi.useRealTimers()
})

const keys = (s: Parameters<typeof bannersOf>[0]) => bannersOf(s).map((b) => b.key)

describe('the banners, most severe first', () => {
  test('every banner at once', () => {
    const state: State = {
      ...st,
      mode: 'observe',
      complete: false,
      writerVerdict: 'foreign',
      credentials: [],
    }
    const s = appState({
      state,
      conn: 'web-down',
      connSince: '2026-10-01T12:01:30Z',
      upstream: { up: false, since: '2026-10-01T12:01:30Z' },
      link: { state: 'too-many', since: '2026-10-01T12:01:00Z', retryAfter: 30 },
      skew: true,
    })
    expect(keys(s)).toEqual(['web', 'egress', 'writer', 'tabs', 'hold', 'inventory', 'waiting', 'approval', 'observe', 'credential', 'version'])
    // the daemon's banner when the page reaches pco web
    expect(keys({ ...s, conn: 'daemon-down', connSince: s.upstream.since })[0]).toBe('daemon')
  })

  test('the populated fixture', () => {
    expect(keys(appState({ state: st }))).toEqual(['egress', 'hold', 'waiting', 'approval'])
  })

  test('nothing for a state in order', () => {
    expect(keys(appState({ state: untagged as unknown as State }))).toEqual([])
  })

  test('stale data says from when, and how long ago on the browser\'s clock', () => {
    const s = appState({
      state: untagged as unknown as State,
      conn: 'stale',
      connSince: '2026-10-01T12:00:02Z',
      times: { at: '2026-10-01T12:00:00Z', finishedAt: '2026-10-01T12:00:02Z', digest: 'd' },
      receivedAt: 1000,
    })
    expect(keys(s)).toEqual(['stale'])
    const [b] = bannersOf(s, 41_000)
    const { container } = render(<>{b?.text}</>)
    expect(container.textContent).toMatch(/^The data is from .+, 40 s ago: no cycle of the daemon has finished since\./)
  })
})

test('the egress banner carries the command, to run as root', async () => {
  const { store } = await fakeStore({ state: populated })
  render(
    <StoreProvider store={store}>
      <Banners />
    </StoreProvider>,
  )
  const egress = document.querySelector('[data-banner="egress"]')
  expect(egress?.textContent).toContain('The egress filter is switched off since')
  expect(egress?.textContent).toContain('the connectors are not confined.')
  expect(egress?.querySelector('code')?.textContent).toBe('pco egress on')
  expect(egress?.textContent).toContain('Run it as root on the node.')
  expect(egress?.getAttribute('role')).toBe('alert')
})

test('the egress filter not loaded asks for pco egress load', () => {
  const s = appState({ state: { ...st, egress: { state: 'not loaded' } } })
  const [b] = bannersOf(s)
  const { container } = render(<>{b?.text}</>)
  expect(container.querySelector('code')?.textContent).toBe('pco egress load')
})

test('the reload banner after a hello of another version', async () => {
  const { store } = await fakeStore({ state: untagged })
  render(
    <StoreProvider store={store}>
      <Banners />
    </StoreProvider>,
  )
  expect(screen.queryByText('pco was updated; reload to use it.')).toBeNull()
  const hello = store.get().hello
  if (!hello) throw new Error('no hello')
  store.notice({ kind: 'hello', data: { ...hello, version: 'v9.9.9' } })
  expect(await screen.findByText('pco was updated; reload to use it.')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Reload' })).toBeTruthy()
})

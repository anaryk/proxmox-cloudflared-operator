import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { type AppStore, StoreProvider } from '../api/store'
import type { State, TrafficView } from '../api/types.gen'
import { fakeStore, flush } from '../test/store'
import { collapse } from './collapse'
import { FlowPanel } from './FlowMap'
import { buildModel } from './model'
import type { MotionLoop } from './motion'
import chains from './testdata/chains.json'
import traffic from './testdata/traffic.json'
import type { Model } from './types'

// Every loop the panel makes, its pause, resume and stop watched.
const made = vi.hoisted(() => [] as MotionLoop[])
vi.mock('./motion', async (importOriginal) => {
  const real = await importOriginal<typeof import('./motion')>()
  return {
    ...real,
    createMotion: (opts: Parameters<typeof real.createMotion>[0]) => {
      const loop = real.createMotion(opts)
      const watched: MotionLoop = { ...loop, pause: vi.fn(loop.pause), resume: vi.fn(loop.resume), stop: vi.fn(loop.stop) }
      made.push(watched)
      return watched
    },
  }
})

const st = chains as unknown as State
const tv = traffic as unknown as TrafficView
const viewOf = (s: State): Model => collapse(buildModel(s, tv), { expanded: new Set() }).model

afterEach(() => {
  made.length = 0
  vi.restoreAllMocks()
})

async function show(model: Model = viewOf(st)) {
  const { store } = await fakeStore({ state: st, answers: { 'GET /api/v1/traffic': () => ({ status: 200, body: tv }) } })
  await flush()
  const panel = (m: Model) => (
    <StoreProvider store={store}>
      <FlowPanel model={m} onSelect={() => undefined} onExpand={() => undefined} paused={false} />
    </StoreProvider>
  )
  const r = render(panel(model))
  const loop = made.at(-1)
  if (!loop) throw new Error('the panel made no loop')
  return { store, loop, r, rerender: (m: Model) => r.rerender(panel(m)) }
}

const legendSays = () => screen.getByRole('list', { name: 'Legend' }).querySelector('.fm-legend-moves')?.textContent
const item = (id: string) => [...document.querySelectorAll<HTMLElement>('[data-item]')].find((x) => x.dataset.item === id)

function reducedQuery(matches: boolean) {
  const listeners = new Set<() => void>()
  const query = {
    matches,
    media: '(prefers-reduced-motion: reduce)',
    addEventListener: (_: string, f: () => void) => listeners.add(f),
    removeEventListener: (_: string, f: () => void) => listeners.delete(f),
  }
  vi.spyOn(window, 'matchMedia').mockImplementation((q: string) =>
    (q === query.media ? query : { matches: false, media: q, addEventListener: () => undefined, removeEventListener: () => undefined }) as unknown as MediaQueryList,
  )
  return {
    set(next: boolean) {
      query.matches = next
      for (const f of listeners) f()
    },
  }
}

describe('the map of the Overview', () => {
  test('a page without new data pauses the loop and greys the figures; live again, it moves again', async () => {
    const { store, loop } = await show()
    expect(loop.pause).not.toHaveBeenCalled()
    expect(document.querySelector('.fm-frame')?.classList.contains('fm-stale')).toBe(false)
    act(() => (store as AppStore).link({ state: 'reconnecting', since: new Date().toISOString() }))
    expect(loop.pause).toHaveBeenCalled()
    expect(document.querySelector('.fm-frame')?.classList.contains('fm-stale')).toBe(true)
    expect(legendSays()).toBe('No new data: the figures are the last ones read, and nothing moves.')
    const resumed = vi.mocked(loop.resume).mock.calls.length
    act(() => store.link({ state: 'open', since: new Date().toISOString() }))
    expect(vi.mocked(loop.resume).mock.calls.length).toBeGreaterThan(resumed)
    expect(document.querySelector('.fm-frame')?.classList.contains('fm-stale')).toBe(false)
    store.stop()
  })

  test('the loop ends with the map, its listeners with it', async () => {
    const removed = vi.spyOn(document, 'removeEventListener')
    const { store, loop, r } = await show()
    expect(loop.stop).not.toHaveBeenCalled()
    r.unmount()
    expect(loop.stop).toHaveBeenCalled()
    expect(removed.mock.calls.some(([type]) => type === 'visibilitychange')).toBe(true)
    const frames = vi.spyOn(window, 'requestAnimationFrame')
    document.dispatchEvent(new Event('visibilitychange'))
    expect(frames).not.toHaveBeenCalled()
    store.stop()
  })

  test('reduced motion, asked by the system: chevrons and figures, and back to dots when it changes', async () => {
    const reduced = reducedQuery(true)
    const { store } = await show()
    expect(document.querySelectorAll('.fm-chevrons').length).toBeGreaterThan(0)
    expect(legendSays()).toBe('Reduced motion: chevrons and the figure stand for the dots.')
    act(() => reduced.set(false))
    expect(document.querySelectorAll('.fm-chevrons')).toHaveLength(0)
    expect(legendSays()).toBe('Only lines with a measured figure move: dot density follows the rate, errors are red diamonds. Hostname lines never move.')
    store.stop()
  })

  test('a new view keeps the focus and the tab stop where they were, and rings what changed', async () => {
    const { store, rerender } = await show()
    const row = item('route:www.example.com qemu/101')
    act(() => row?.focus())
    expect(document.activeElement).toBe(row)
    const routes = st.routes.map((r) => (r.hostname === 'api.example.com' ? { ...r, state: 'withdrawn', reason: 'identity check failed' } : r))
    rerender(viewOf({ ...st, routes }))
    expect(item('route:www.example.com qemu/101')).toBe(row)
    expect(document.activeElement).toBe(row)
    expect(row?.getAttribute('tabindex')).toBe('0')
    expect(document.querySelectorAll('.fm-frame [tabindex="0"]')).toHaveLength(1)
    expect(item('route:api.example.com qemu/103')?.querySelector('.fm-ring')).not.toBeNull()
    store.stop()
  })
})

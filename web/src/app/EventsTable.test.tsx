import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import type { Event, GapNotice } from '../api/types.gen'
import events from '../fixtures/events.json'
import hello from '../fixtures/hello.json'
import untagged from '../fixtures/untagged.json'
import { fakeStore, flush } from '../test/store'
import { EventsStrip } from './EventsStrip'
import { EventsTable, matches, rowsOf } from './EventsTable'

const evs = events as Event[]
const boot = hello.boot
const gap: GapNotice = { boot, from: 11, to: 13, count: 3, level: 'error' }

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('rowsOf', () => {
  test('newest first, a gap in its place', () => {
    const rows = rowsOf(evs, [gap], new Map(), {})
    expect(rows.map((r) => (r.type === 'gap' ? `gap ${r.g.to}` : r.e.seq))).toEqual(['gap 13', 10, 9, 8, 7])
  })

  test('the filter of the Events page', () => {
    expect(rowsOf(evs, [gap], new Map(), { kind: ['rollout', 'admin'] }).map((r) => (r.type === 'event' ? r.e.seq : 'gap'))).toEqual([10, 8])
    expect(rowsOf(evs, [gap], new Map(), { route: ['www.example.com'] }).map((r) => (r.type === 'event' ? r.e.seq : 'gap'))).toEqual([7])
    expect(rowsOf(evs, [gap], new Map(), { level: ['error'] }).map((r) => r.type)).toEqual(['gap'])
    expect(matches(evs[3] as Event, { text: 'ALICE' })).toBe(true)
  })

  test('paused: nothing after the last event there was', () => {
    expect(rowsOf(evs, [gap], new Map(), {}, 9).map((r) => (r.type === 'event' ? r.e.seq : 'gap'))).toEqual([9, 8, 7])
  })

  test('an opened gap is its events; one loaded in part keeps its row', () => {
    const loaded = [11, 12, 13].map((seq) => ({ ...(evs[0] as Event), seq }))
    const whole = rowsOf(evs, [gap], new Map([[`gap:${boot}:11:13`, { events: loaded, complete: true }]]), {})
    expect(whole.map((r) => (r.type === 'event' ? r.e.seq : 'gap'))).toEqual([13, 12, 11, 10, 9, 8, 7])
    const part = rowsOf(evs, [gap], new Map([[`gap:${boot}:11:13`, { events: loaded.slice(1), complete: false }]]), {})
    expect(part.map((r) => (r.type === 'event' ? r.e.seq : 'gap')).sort()).toEqual([10, 12, 13, 7, 8, 9, 'gap'].sort())
  })
})

test('a gap row loads its events when opened', async () => {
  const loaded = [11, 12, 13, 14].map((seq) => ({ ...(evs[0] as Event), seq, boot, message: `event ${seq}` }))
  const { store } = await fakeStore({ state: untagged })
  store.notice({ kind: 'gap', data: gap })
  const fetch = vi.fn<(url: string) => Promise<Response>>(async () => new Response(JSON.stringify(loaded), { status: 200 }))
  vi.stubGlobal('fetch', fetch)
  render(
    <StoreProvider store={store}>
      <EventsTable filter={{}} live />
    </StoreProvider>,
  )
  const row = screen.getByText(/3 events of one cycle/).closest('tr')
  if (!row) throw new Error('no gap row')
  expect(row.className).toContain('row-error')
  fireEvent.click(row)
  expect(await screen.findByText('event 12')).toBeTruthy()
  expect(fetch.mock.calls[0]?.[0]).toBe(`/api/v1/events?after=10&boot=${boot}&limit=503`)
  // what came after the gap is not the gap's
  expect(screen.queryByText('event 14')).toBeNull()
  expect(fetch).toHaveBeenCalledTimes(1)
})

describe('a gap older than what the daemon holds in memory', () => {
  const ev = (seq: number) => ({ ...(evs[0] as Event), seq, boot, message: `event ${seq}` })

  async function opened(answers: Event[][]) {
    const { store } = await fakeStore({ state: untagged })
    store.notice({ kind: 'gap', data: gap })
    const fetch = vi.fn<(url: string) => Promise<Response>>(async () => new Response(JSON.stringify(answers.shift() ?? []), { status: 200 }))
    vi.stubGlobal('fetch', fetch)
    render(
      <StoreProvider store={store}>
        <EventsTable filter={{}} live />
      </StoreProvider>,
    )
    const open = () => {
      const row = screen.getByText(/events of one cycle/).closest('tr')
      if (!row) throw new Error('no gap row')
      fireEvent.click(row)
    }
    return { fetch, open }
  }

  test('is read from the log as well, and is whole', async () => {
    const { fetch, open } = await opened([[ev(12), ev(13), ev(14)], [ev(11), ev(12), ev(13), ev(14)]])
    open()
    expect(await screen.findByText('event 11')).toBeTruthy()
    expect(fetch.mock.calls.map((c) => c[0])).toEqual([
      `/api/v1/events?after=10&boot=${boot}&limit=503`,
      `/api/v1/events?after=10&boot=${boot}&limit=5000&history=1`,
    ])
    expect(screen.queryByText(/of one cycle/)).toBeNull()
  })

  test('says how much of it there is when the log does not reach back either, and loads again when opened', async () => {
    const { fetch, open } = await opened([[ev(12), ev(13)], [ev(12), ev(13)], [ev(11), ev(12), ev(13)]])
    open()
    expect(
      await screen.findByText(/2 of 3 events of one cycle loaded: the daemon no longer holds the rest, the journal on the node has them\./),
    ).toBeTruthy()
    expect(screen.getByText('event 12')).toBeTruthy()
    open()
    expect(await screen.findByText('event 11')).toBeTruthy()
    expect(fetch.mock.calls.at(-1)?.[0]).toBe(`/api/v1/events?after=10&boot=${boot}&limit=5000&history=1`)
    expect(fetch).toHaveBeenCalledTimes(3)
  })
})

test("a row's detail has the times of the tooltip, for the keyboard", async () => {
  const { store } = await fakeStore({ state: untagged })
  render(
    <StoreProvider store={store}>
      <EventsTable filter={{}} live />
    </StoreProvider>,
  )
  const row = screen.getByText('adoption requested for the next run').closest('tr')
  if (!row) throw new Error('no row')
  fireEvent.keyDown(row, { key: 'Enter' })
  const drawer = screen.getByRole('complementary')
  expect(within(drawer).getByText('On the node (Europe/Prague)')).toBeTruthy()
  expect(within(drawer).getByText('UTC').nextElementSibling?.textContent).toBe('2026-10-01 12:00:00')
  expect(within(drawer).getByText('alice@pve (ticket)')).toBeTruthy()
})

test('a gap with level error colours the strip', async () => {
  const { store } = await fakeStore({ state: untagged })
  const { container } = render(
    <StoreProvider store={store}>
      <EventsStrip />
    </StoreProvider>,
  )
  const strip = container.querySelector('details')
  expect(strip?.className).toBe('strip')
  store.notice({ kind: 'gap', data: { ...gap, level: 'warn' } })
  await flush()
  expect(strip?.className).toBe('strip')
  store.notice({ kind: 'gap', data: { ...gap, from: 14, to: 20, count: 7 } })
  await flush()
  expect(strip?.className).toBe('strip strip-error')
  expect(strip?.querySelector('summary')?.textContent).toContain('errors')
})

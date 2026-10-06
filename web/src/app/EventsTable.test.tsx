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
    expect(rowsOf(evs, [gap], new Map(), {}, { seq: 9 }).map((r) => (r.type === 'event' ? r.e.seq : 'gap'))).toEqual([9, 8, 7])
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

// The count of a gap a reader is told of is that of its seqs, among them the
// events the reader may not see: its row does not say it holds that many, and
// does not blame the daemon for the events that are not shown.
test('a gap of a reader says up to that many events, and why some are missing', async () => {
  const { store } = await fakeStore({ state: untagged, session: { role: 'reader' } })
  store.notice({ kind: 'gap', data: gap })
  const ev = (seq: number) => ({ ...(evs[0] as Event), seq, boot, message: `event ${seq}` })
  const fetch = vi.fn<(url: string) => Promise<Response>>(async () => new Response(JSON.stringify([ev(12)]), { status: 200 }))
  vi.stubGlobal('fetch', fetch)
  render(
    <StoreProvider store={store}>
      <EventsTable filter={{}} live />
    </StoreProvider>,
  )
  const row = screen.getByText(/Up to 3 events of one cycle/).closest('tr')
  if (!row) throw new Error('no gap row')
  fireEvent.click(row)
  expect(await screen.findByText(/1 of up to 3 events of one cycle loaded: the rest are not yours to see, or the daemon no longer holds them\./)).toBeTruthy()
  expect(screen.getByText('event 12')).toBeTruthy()
  expect(screen.queryByText(/journal on the node/)).toBeNull()
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

describe('the range of time', () => {
  const at = (e: Event, when: string): Event => ({ ...e, at: when })
  const timed = [at(evs[0] as Event, '2026-10-01T12:00:00Z'), at(evs[1] as Event, '2026-10-01T12:05:00Z'), at(evs[2] as Event, '2026-10-01T12:10:00Z')]

  test('an event at either end is in it', () => {
    const seqs = (f: { since?: string; until?: string }) => timed.filter((e) => matches(e, f)).map((e) => e.seq)
    expect(seqs({ since: '2026-10-01T12:05:00Z' })).toEqual([8, 9])
    expect(seqs({ until: '2026-10-01T12:05:00Z' })).toEqual([7, 8])
    expect(seqs({ since: '2026-10-01T12:01:00Z', until: '2026-10-01T12:09:00Z' })).toEqual([8])
  })

  test('a bound that is no time sets none', () => {
    expect(timed.filter((e) => matches(e, { since: 'yesterday', until: '' }))).toHaveLength(3)
  })

  test('an event without a time is in no range', () => {
    expect(matches({ ...(evs[0] as Event), at: undefined }, { since: '2026-10-01T12:00:00Z' })).toBe(false)
  })

  test('a gap says no time, so a range does not show it', () => {
    expect(rowsOf(timed, [gap], new Map(), { since: '2026-10-01T11:00:00Z' }).map((r) => r.type)).toEqual(['event', 'event', 'event'])
    expect(rowsOf(timed, [gap], new Map(), {}).map((r) => r.type)).toEqual(['gap', 'event', 'event', 'event'])
  })
})

describe('events of an earlier process of the daemon', () => {
  const earlier = (seq: number, at: string): Event => ({ ...(evs[0] as Event), boot: 'ab12cd34ef567890', seq, at, message: `earlier ${seq}` })
  // their seq is higher than those of the process now running
  const before = [earlier(900, '2026-09-30T10:00:00Z'), earlier(901, '2026-09-30T10:00:05Z')]
  const seqs = (rows: ReturnType<typeof rowsOf>) => rows.map((r) => (r.type === 'event' ? r.e.seq : 'gap'))

  test('come after the events of the latest, whatever their seq', () => {
    expect(seqs(rowsOf([...before, ...evs], [], new Map(), {}))).toEqual([10, 9, 8, 7, 901, 900])
  })

  test('are not cut when the list is paused', () => {
    expect(seqs(rowsOf([...before, ...evs], [], new Map(), {}, { seq: 8 }))).toEqual([8, 7, 901, 900])
  })

  test('an event the stream and the log both have is one row', () => {
    expect(seqs(rowsOf([...evs, ...evs.slice(1)], [], new Map(), {}))).toEqual([10, 9, 8, 7])
  })

  test('a gap of the process now running stays above them', () => {
    expect(seqs(rowsOf([...before, ...evs], [gap], new Map(), {}))).toEqual(['gap', 10, 9, 8, 7, 901, 900])
  })
})

test('the table has the older events its page read, and says what is empty in its own words', async () => {
  const { store } = await fakeStore({ state: untagged })
  const older: Event[] = [{ ...(evs[0] as Event), boot: 'ab12cd34ef567890', seq: 3, message: 'from before the restart' }]
  const { rerender } = render(
    <StoreProvider store={store}>
      <EventsTable filter={{}} live older={older} />
    </StoreProvider>,
  )
  expect(screen.getByText('from before the restart')).toBeTruthy()
  rerender(
    <StoreProvider store={store}>
      <EventsTable filter={{ text: 'nothing says this' }} live older={older} empty="No event matches." />
    </StoreProvider>,
  )
  expect(screen.getByText('No event matches.')).toBeTruthy()
})

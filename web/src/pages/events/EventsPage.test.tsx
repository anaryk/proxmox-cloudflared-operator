import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { StoreProvider } from '../../api/store'
import type { Event, GapNotice } from '../../api/types.gen'
import { navigate } from '../../app/router'
import { ToastProvider } from '../../components/Toast'
import events from '../../fixtures/events.json'
import hello from '../../fixtures/hello.json'
import untagged from '../../fixtures/untagged.json'
import { fakeStore, flush } from '../../test/store'
import { cutText, EventsPage, everyText } from './EventsPage'
import { localInput } from './filter'

const evs = events as Event[]
const boot = hello.boot
const earlier = 'ab12cd34ef567890'

const older = (seq: number, patch: Partial<Event> = {}): Event => ({
  ...(evs[0] as Event),
  boot: earlier,
  seq,
  at: '2026-09-30T10:00:00Z',
  message: `before the restart ${seq}`,
  ...patch,
})

function answers(...bodies: Event[][]) {
  const fetch = vi.fn<(url: string) => Promise<Response>>(async () => new Response(JSON.stringify(bodies.shift() ?? []), { status: 200 }))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

async function mount() {
  const fake = await fakeStore({ state: untagged })
  const view = render(
    <StoreProvider store={fake.store}>
      <ToastProvider>
        <EventsPage />
      </ToastProvider>
    </StoreProvider>,
  )
  return { ...fake, ...view }
}

const input = (label: string) => screen.getByLabelText(label) as HTMLInputElement

beforeEach(() => {
  navigate('/events')
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  navigate('/')
})

describe('the filters and the address', () => {
  test('each filter goes to the address', async () => {
    await mount()
    fireEvent.click(screen.getByLabelText('error'))
    fireEvent.click(screen.getByLabelText('warn'))
    expect(window.location.search).toBe('?level=error&level=warn')
    fireEvent.click(screen.getByText('Kind', { selector: 'summary' }))
    fireEvent.click(screen.getByLabelText('rollout'))
    fireEvent.change(input('Route'), { target: { value: 'www.example.com' } })
    fireEvent.change(input('Guest'), { target: { value: 'qemu/101' } })
    fireEvent.change(input('Account'), { target: { value: 'acc1' } })
    fireEvent.change(input('Text'), { target: { value: 'alice' } })
    const q = new URLSearchParams(window.location.search)
    expect(q.getAll('level')).toEqual(['error', 'warn'])
    expect(q.getAll('kind')).toEqual(['rollout'])
    expect(q.get('route')).toBe('www.example.com')
    expect(q.get('guest')).toBe('qemu/101')
    expect(q.get('account')).toBe('acc1')
    expect(q.get('text')).toBe('alice')
  })

  test('the address sets the filters, and the list follows it', async () => {
    navigate('/events?account=acc1&kind=rollout&kind=action&text=connectors')
    await mount()
    expect(input('Account').value).toBe('acc1')
    expect(input('Text').value).toBe('connectors')
    expect(screen.getByText(/configuration version 3 runs on 2 connectors/)).toBeTruthy()
    expect(screen.queryByText(/3 rules replace version 2/)).toBeNull()
    expect(screen.queryByText('qemu/101: unreachable')).toBeNull()
    act(() => navigate('/events?account=acc1&kind=rollout&kind=action'))
    expect(input('Text').value).toBe('')
    expect(screen.getByText(/3 rules replace version 2/)).toBeTruthy()
    expect(screen.getByText(/configuration version 3 runs on 2 connectors/)).toBeTruthy()
    expect(screen.queryByText('qemu/101: unreachable')).toBeNull()
  })

  test('an address with several values of a list shows them all, and a new one adds to them', async () => {
    navigate('/events?route=a.example.com&route=b.example.com')
    await mount()
    expect(input('Route').value).toBe('a.example.com, b.example.com')
    fireEvent.change(input('Route'), { target: { value: 'a.example.com, b.example.com, ' } })
    expect(input('Route').value).toBe('a.example.com, b.example.com, ')
    fireEvent.change(input('Route'), { target: { value: 'a.example.com, b.example.com, c.example.com' } })
    expect(new URLSearchParams(window.location.search).getAll('route')).toEqual(['a.example.com', 'b.example.com', 'c.example.com'])
  })

  test('the range of time is in the address as RFC 3339, in the browser zone in the field', async () => {
    await mount()
    fireEvent.change(input('From'), { target: { value: '2026-10-01T12:00' } })
    fireEvent.change(input('To'), { target: { value: '2026-10-01T12:05' } })
    const q = new URLSearchParams(window.location.search)
    expect(q.get('since')).toBe(new Date('2026-10-01T12:00').toISOString().replace('.000Z', 'Z'))
    // to the end of the minute
    expect(q.get('until')).toBe(new Date(new Date('2026-10-01T12:05').getTime() + 59_999).toISOString())
    expect(input('From').value).toBe('2026-10-01T12:00')
    expect(input('To').value).toBe('2026-10-01T12:05')
    fireEvent.change(input('From'), { target: { value: '' } })
    expect(new URLSearchParams(window.location.search).has('since')).toBe(false)
  })

  test('only events in the range show', async () => {
    const at = (seq: number, when: string): Event => ({ ...(evs[0] as Event), seq, boot, at: when, message: `at ${when}` })
    navigate('/events?since=2026-10-01T12:01:00Z&until=2026-10-01T12:03:00Z')
    const { store } = await mount()
    act(() => {
      store.notice({ kind: 'event', data: at(20, '2026-10-01T12:00:30Z') })
      store.notice({ kind: 'event', data: at(21, '2026-10-01T12:02:00Z') })
      store.notice({ kind: 'event', data: at(22, '2026-10-01T12:04:00Z') })
    })
    expect(screen.getByText('at 2026-10-01T12:02:00Z')).toBeTruthy()
    expect(screen.queryByText('at 2026-10-01T12:00:30Z')).toBeNull()
    expect(screen.queryByText('at 2026-10-01T12:04:00Z')).toBeNull()
    expect(input('From').value).toBe(localInput('2026-10-01T12:01:00Z'))
  })

  test('clearing the filters empties the address', async () => {
    navigate('/events?level=error&text=x')
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Clear the filters' }))
    expect(window.location.pathname + window.location.search).toBe('/events')
    expect(screen.queryByRole('button', { name: 'Clear the filters' })).toBeNull()
  })

  test('says why the list is empty when a filter is', async () => {
    navigate('/events?text=nothing+says+this')
    await mount()
    expect(screen.getByText(/No event among those the page has read passes these filters/)).toBeTruthy()
  })

  test('offers the kinds of the events it has, besides those it knows', async () => {
    const { store } = await mount()
    act(() => store.notice({ kind: 'event', data: { ...(evs[0] as Event), seq: 30, boot, kind: 'newkind' } }))
    fireEvent.click(screen.getByText('Kind', { selector: 'summary' }))
    expect(screen.getByLabelText('newkind')).toBeTruthy()
    expect(screen.getByLabelText('credential')).toBeTruthy()
  })
})

describe('a gap', () => {
  test('loads the events of its range when it is opened', async () => {
    const gap: GapNotice = { boot, from: 11, to: 13, count: 3, level: 'error' }
    const loaded = [11, 12, 13].map((seq) => ({ ...(evs[0] as Event), seq, boot, message: `event ${seq}` }))
    const fetch = answers(loaded)
    const { store } = await mount()
    act(() => store.notice({ kind: 'gap', data: gap }))
    fireEvent.click(screen.getByText(/3 events of one cycle/))
    expect(await screen.findByText('event 12')).toBeTruthy()
    expect(fetch.mock.calls[0]?.[0]).toBe(`/api/v1/events?after=10&boot=${boot}&limit=503`)
  })

  test('opened after the log was read, keeps its events when the filter changes', async () => {
    navigate('/events?kind=route')
    const gap: GapNotice = { boot, from: 11, to: 13, count: 3, level: 'warn' }
    const inGap = [11, 12, 13].map((seq) => ({ ...(evs[0] as Event), seq, boot, kind: 'route', message: `in the gap ${seq}` }))
    // the read of the log holds the gap's events; the gap's own read too
    const fetch = answers(inGap, inGap)
    const { store } = await mount()
    act(() => store.notice({ kind: 'gap', data: gap }))
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    expect(await screen.findByText('in the gap 12')).toBeTruthy()
    // the log brought every event of the gap: it offers to load nothing
    expect(screen.queryByText(/events of one cycle/)).toBeNull()
    act(() => navigate('/events?kind=route&kind=action'))
    // what was read of the log is gone with the filter; the gap is back
    fireEvent.click(await screen.findByText(/3 events of one cycle/))
    expect(await screen.findByText('in the gap 12')).toBeTruthy()
    expect(screen.getByText('in the gap 11')).toBeTruthy()
    expect(screen.getByText('in the gap 13')).toBeTruthy()
    expect(fetch).toHaveBeenCalledTimes(2)
  })

  test('opened, keeps its events when the filter changes after the log was read', async () => {
    navigate('/events?kind=route')
    const gap: GapNotice = { boot, from: 11, to: 13, count: 3, level: 'warn' }
    const inGap = [11, 12, 13].map((seq) => ({ ...(evs[0] as Event), seq, boot, kind: 'route', message: `in the gap ${seq}` }))
    answers(inGap.slice(0, 2), inGap)
    const { store } = await mount()
    act(() => store.notice({ kind: 'gap', data: gap }))
    // the log has two of the three, so the gap still offers them
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    expect(await screen.findByText('in the gap 12')).toBeTruthy()
    fireEvent.click(screen.getByText(/3 events of one cycle/))
    expect(await screen.findByText('in the gap 13')).toBeTruthy()
    act(() => navigate('/events?kind=route&kind=action'))
    for (const seq of [11, 12, 13]) expect(screen.getByText(`in the gap ${seq}`)).toBeTruthy()
  })

  test('is not shown while a filter names a route', async () => {
    navigate('/events?route=www.example.com')
    const { store } = await mount()
    act(() => store.notice({ kind: 'gap', data: { boot, from: 11, to: 13, count: 3, level: 'error' } }))
    expect(screen.queryByText(/events of one cycle/)).toBeNull()
  })
})

describe('Load older events', () => {
  test('reads the log with the filters of the page, and again for more', async () => {
    navigate('/events?level=warn&account=acc1&since=2026-09-01T00:00:00Z&text=restart')
    const fetch = answers(
      [older(5), older(6)],
      Array.from({ length: 2000 }, (_, i) => older(100 + i)),
    )
    await mount()
    expect(fetch).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    expect(await screen.findByText('before the restart 5')).toBeTruthy()
    expect(screen.getByText('before the restart 6')).toBeTruthy()
    const url = new URL(fetch.mock.calls[0]?.[0] ?? '', 'https://page.invalid')
    expect(url.pathname).toBe('/api/v1/events')
    expect(url.searchParams.get('history')).toBe('1')
    expect(url.searchParams.get('limit')).toBe('1000')
    expect(url.searchParams.getAll('level')).toEqual(['warn'])
    expect(url.searchParams.getAll('account')).toEqual(['acc1'])
    expect(url.searchParams.get('since')).toBe('2026-09-01T00:00:00Z')
    // the text is the page's to apply
    expect(url.searchParams.has('text')).toBe(false)
    // fewer than asked for: that is all there is in what the daemon reads
    const button = screen.getByRole('button', { name: 'Load older events' })
    expect(button.getAttribute('aria-disabled')).toBe('true')
    expect(screen.getByText(everyText)).toBeTruthy()
    fetch.mockClear()
    fireEvent.click(button)
    await flush()
    expect(fetch).not.toHaveBeenCalled()
  })

  test('asks for 1000 more each time, up to what the daemon answers', async () => {
    const many = (n: number) => Array.from({ length: n }, (_, i) => older(i + 1))
    const fetch = answers(many(1000), many(2000), many(3000), many(4000), many(5000))
    await mount()
    for (const limit of [1000, 2000, 3000, 4000, 5000]) {
      fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
      await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(limit / 1000))
      await flush()
      expect(new URL(fetch.mock.calls.at(-1)?.[0] ?? '', 'https://page.invalid').searchParams.get('limit')).toBe(String(limit))
    }
    expect(screen.getByText(`${cutText} Narrow the filters to see them.`)).toBeTruthy()
  })

  test('a read as long as it asked for says that the list may go on beyond it', async () => {
    answers(Array.from({ length: 1000 }, (_, i) => older(i + 1)))
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    expect(await screen.findByText('The list ends at the oldest of the 1000 newest events of these filters read so far.')).toBeTruthy()
  })

  test('a range in the past reads the newest events up to its end', async () => {
    navigate('/events?until=2026-09-30T11:00:00Z')
    const fetch = answers([older(5)])
    await mount()
    expect(screen.getByText('Load older events reads the newest events up to it from the log on the node.')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    expect(await screen.findByText('before the restart 5')).toBeTruthy()
    expect(new URL(fetch.mock.calls[0]?.[0] ?? '', 'https://page.invalid').searchParams.get('until')).toBe('2026-09-30T11:00:00Z')
  })

  test('forgets what it read when the filters change', async () => {
    answers([older(5)])
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    expect(await screen.findByText('before the restart 5')).toBeTruthy()
    fireEvent.click(screen.getByLabelText('info'))
    expect(screen.queryByText('before the restart 5')).toBeNull()
    expect(screen.getByRole('button', { name: 'Load older events' }).getAttribute('aria-disabled')).toBeNull()
  })

  test('puts the events of an earlier run below those now, whatever their seq', async () => {
    answers([older(900), older(901)])
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    await screen.findByText('before the restart 901')
    const rows = screen.getAllByRole('row').map((r) => r.textContent ?? '')
    const at = (text: string) => rows.findIndex((r) => r.includes(text))
    expect(at('adoption requested for the next run')).toBeLessThan(at('before the restart 901'))
    expect(at('before the restart 901')).toBeLessThan(at('before the restart 900'))
  })

  test('says what the daemon said when it cannot read the log', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ code: 'unavailable', error: 'reading the event log: i/o error' }), { status: 503 })),
    )
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    expect((await screen.findByRole('alert')).textContent).toContain('reading the event log: i/o error')
    expect(screen.getByRole('button', { name: 'Load older events' }).getAttribute('aria-disabled')).toBeNull()
  })
})

describe('Live', () => {
  const next = { ...(evs[0] as Event), seq: 40, boot, message: 'a new event' }

  test('adds the events that come', async () => {
    const { store } = await mount()
    act(() => store.notice({ kind: 'event', data: next }))
    expect(screen.getByText('a new event')).toBeTruthy()
  })

  test('off right after a restart, still shows what the process before wrote', async () => {
    const fetch = answers([older(900), older(901)])
    const { store } = await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }))
    await screen.findByText('before the restart 901')
    // the daemon starts again: nothing of the new process yet, its seq is small
    fetch.mockImplementation(async () => new Response('[]', { status: 200 }))
    await act(async () => {
      store.notice({ kind: 'hello', data: { ...hello, boot: 'fe00000000000001', seq: 2 } })
      await flush()
    })
    fireEvent.click(screen.getByRole('switch', { name: 'Live' }))
    expect(screen.getByText('before the restart 901')).toBeTruthy()
    expect(screen.getByText('before the restart 900')).toBeTruthy()
  })

  test('off, holds the list as it was until it is on again', async () => {
    const { store } = await mount()
    fireEvent.click(screen.getByRole('switch', { name: 'Live' }))
    expect(screen.getByText(/Paused/)).toBeTruthy()
    act(() => store.notice({ kind: 'event', data: next }))
    expect(screen.queryByText('a new event')).toBeNull()
    fireEvent.click(screen.getByRole('switch', { name: 'Live' }))
    expect(screen.getByText('a new event')).toBeTruthy()
    expect(screen.queryByText(/Paused/)).toBeNull()
  })
})

describe('Export the list', () => {
  function capture() {
    const blobs: Blob[] = []
    const names: string[] = []
    Object.assign(URL, {
      createObjectURL: (b: Blob) => {
        blobs.push(b)
        return 'blob:test'
      },
      revokeObjectURL: () => {},
    })
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      names.push(this.download)
    })
    return { blobs, names }
  }

  test('is what the log has for the filters, with the page applying the text', async () => {
    navigate('/events?kind=route&kind=admin&text=alice')
    const all = [...evs, older(3, { kind: 'admin', actor: 'alice@pve (ticket)' }), older(4, { kind: 'admin' })]
    const fetch = answers(all)
    const { blobs, names } = capture()
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Export the list' }))
    await vi.waitFor(() => expect(blobs).toHaveLength(1))
    const url = new URL(fetch.mock.calls[0]?.[0] ?? '', 'https://page.invalid')
    expect(url.searchParams.get('limit')).toBe('5000')
    expect(url.searchParams.get('history')).toBe('1')
    expect(url.searchParams.getAll('kind')).toEqual(['route', 'admin'])
    const exported = JSON.parse((await blobs[0]?.text()) ?? '') as Event[]
    expect(exported.map((e) => e.seq)).toEqual([10, 3])
    expect(names[0]).toMatch(/^pco-events-pve1-\d{8}-\d{6}\.json$/)
    expect(screen.getByText(/Exported 2 events to pco-events-pve1-/)).toBeTruthy()
  })

  test('a range in the past is read up to its end, and a file the daemon cut says so', async () => {
    navigate('/events?until=2026-09-30T11:00:00Z')
    const fetch = answers(Array.from({ length: 5000 }, (_, i) => older(i + 1)))
    capture()
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Export the list' }))
    expect(await screen.findByText(/^Exported 5000 events to pco-events-pve1-/)).toBeTruthy()
    expect(screen.getByText(/^Exported 5000 events/).textContent).toContain(cutText)
    expect(new URL(fetch.mock.calls[0]?.[0] ?? '', 'https://page.invalid').searchParams.get('until')).toBe('2026-09-30T11:00:00Z')
  })

  test('says so when the log cannot be read', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ code: 'unavailable', error: 'no log' }), { status: 503 })))
    const { blobs } = capture()
    await mount()
    fireEvent.click(screen.getByRole('button', { name: 'Export the list' }))
    expect(await screen.findByText(/The events were not exported: no log/)).toBeTruthy()
    expect(blobs).toHaveLength(0)
  })
})

test('reads nothing of its own until it is asked to', async () => {
  const fetch = answers()
  const { calls } = await mount()
  expect(fetch).not.toHaveBeenCalled()
  expect(calls.filter((c) => c.path.startsWith('/api/v1/events'))).toHaveLength(1)
})

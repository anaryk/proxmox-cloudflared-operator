import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import events from '../fixtures/events.json'
import hello from '../fixtures/hello.json'
import populated from '../fixtures/populated.json'
import session from '../fixtures/session.json'
import traffic from '../fixtures/traffic.json'
import unauthenticated from '../fixtures/unauthenticated.json'
import { type Answer, ApiError, type Method, type RequestOptions } from './client'
import { AppStore, dataAge, fullFetchEvery, mergeTraffic, shareRoutes, staleAfterMs, type Storages } from './store'
import type { Shared } from './leader'
import { type Link, type Notice, resetTraffic } from './stream'
import type { Event, State, TrafficView } from './types.gen'

interface Call {
  method: Method
  path: string
  body?: unknown
  opts?: RequestOptions
}

type Reply = (c: Call) => Answer | ApiError

function memory(): Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> {
  const m = new Map<string, string>()
  return {
    getItem: (k) => m.get(k) ?? null,
    setItem: (k, v) => void m.set(k, v),
    removeItem: (k) => void m.delete(k),
  }
}

let now: number // the browser's time
let mono: number // its monotonic clock
let calls: Call[]
let replies: Map<string, Reply>
let storages: Storages
let shared: { opened: number; resumed: number; closed: number; link?: (l: Link) => void }

const ok = (body: unknown, etag?: string): Reply => () => ({ status: 200, body, etag })

function store() {
  return new AppStore({
    now: () => now,
    mono: () => mono,
    storages,
    request: async <T,>(method: Method, path: string, body?: unknown, opts?: RequestOptions) => {
      const c = { method, path, body, opts }
      calls.push(c)
      const reply = replies.get(`${method} ${path.split('?')[0]}`)
      const got = reply ? reply(c) : new ApiError(404, { code: 'not_found', error: 'no fixture' })
      if (got instanceof ApiError) throw got
      return got as Answer<T>
    },
    share: (o): Shared => {
      shared.opened++
      shared.link = o.onLink
      return { close: () => shared.closed++, resume: () => shared.resumed++, leading: () => true }
    },
  })
}

const state = populated as unknown as State
const flush = () => new Promise((r) => setTimeout(r, 0))
// pass lets time go by, on both clocks of the browser
const pass = (ms: number) => {
  now += ms
  mono += ms
}
const paths = () => calls.map((c) => `${c.method} ${c.path}`)

beforeEach(() => {
  now = Date.parse('2026-10-01T12:00:05Z')
  mono = 5000
  calls = []
  storages = { session: memory(), local: memory() }
  shared = { opened: 0, resumed: 0, closed: 0 }
  replies = new Map<string, Reply>([
    ['GET /api/session', ok(session)],
    ['GET /api/v1/state', ok(state, state.digest)],
    ['GET /api/v1/events', ok(events)],
    ['GET /api/v1/traffic', ok(traffic)],
    ['GET /api/v1/settings', ok({ rev: 1, settings: { gateTag: 'cf-tunnel' } })],
  ])
})

afterEach(() => {
  vi.useRealTimers()
})

async function opened() {
  const s = store()
  await s.loadSession()
  s.link({ state: 'open', since: '2026-10-01T12:00:05Z' })
  s.notice({ kind: 'hello', data: hello }, '')
  await flush()
  return s
}

describe('hello', () => {
  test('keeps the boot, version and poll interval, and loads what the page shows', async () => {
    const s = await opened()
    expect(s.get().hello).toEqual(hello)
    expect(s.get().loadedVersion).toBe(hello.version)
    expect(s.get().state?.digest).toBe(populated.digest)
    expect(s.get().events.map((e) => e.seq)).toEqual([7, 8, 9, 10])
    expect(s.get().traffic?.tunnels).toHaveLength(1)
    expect(s.get().settings?.settings.gateTag).toBe('cf-tunnel')
    // the store's own reads are background ones
    expect(calls.filter((c) => c.path.startsWith('/api/v1/')).every((c) => c.opts?.background)).toBe(true)
    expect(shared.opened).toBe(1)
  })

  test('another version than the page was loaded with raises the reload banner', async () => {
    const s = await opened()
    expect(s.get().skew).toBe(false)
    s.notice({ kind: 'hello', data: { ...hello, version: '1.3.0' } })
    expect(s.get().skew).toBe(true)
  })

  // The stream of a reader ends when the guests it sees change, and the
  // page opens another. The digest of the daemon is the same then; the state
  // the reader gets is not.
  test('a stream that begins again reads the state, for the guests of a reader may have changed', async () => {
    const s = await opened()
    expect(s.get().state?.routes.length).toBeGreaterThan(1)
    calls = []
    const fewer = { ...state, routes: state.routes.slice(0, 1) }
    replies.set('GET /api/v1/state', (c) =>
      c.opts?.ifNoneMatch === state.digest ? { status: 200, body: fewer, etag: 'set-2' } : new ApiError(500, { code: 'internal', error: 'not asked with the ETag held' }),
    )
    s.notice({ kind: 'hello', data: hello })
    await flush()
    expect(paths()).toEqual(['GET /api/v1/state'])
    expect(calls[0]?.opts?.ifNoneMatch).toBe(state.digest)
    expect(s.get().state?.routes).toHaveLength(1)
  })

  test('a stream that begins again over the same state costs a 304', async () => {
    const s = await opened()
    const before = s.get().state
    calls = []
    replies.set('GET /api/v1/state', () => ({ status: 304, body: undefined }))
    s.notice({ kind: 'hello', data: hello })
    await flush()
    expect(paths()).toEqual(['GET /api/v1/state'])
    expect(s.get().state).toBe(before)
  })

  test('a new boot drops everything and fetches again', async () => {
    const s = await opened()
    calls = []
    s.notice({ kind: 'hello', data: { ...hello, boot: '0123456789abcdef' } })
    expect(s.get().events).toEqual([])
    await flush()
    expect(paths()).toEqual(expect.arrayContaining(['GET /api/v1/state', 'GET /api/v1/events?limit=500', 'GET /api/v1/traffic']))
  })
})

describe('state notices', () => {
  test('the same digest makes no fetch, and takes the times', async () => {
    const s = await opened()
    calls = []
    const times = { at: '2026-10-01T12:00:10Z', finishedAt: '2026-10-01T12:00:12Z', digest: populated.digest }
    s.notice({ kind: 'state', data: times })
    await flush()
    expect(calls).toEqual([])
    expect(s.get().times).toEqual(times)
  })

  test('a new digest makes a conditional fetch, and a 304 keeps the state', async () => {
    const s = await opened()
    const before = s.get().state
    calls = []
    replies.set('GET /api/v1/state', () => ({ status: 304, body: undefined, etag: populated.digest }))
    s.notice({ kind: 'state', data: { at: '2026-10-01T12:00:10Z', finishedAt: '2026-10-01T12:00:12Z', digest: 'ffffffffffffffff' } })
    await flush()
    expect(calls).toHaveLength(1)
    expect(calls[0]?.opts).toEqual({ background: true, ifNoneMatch: populated.digest })
    expect(s.get().state).toBe(before)
  })

  test('the routes that did not change are the same objects after an update', async () => {
    const s = await opened()
    const [first, second] = s.get().state?.routes ?? []
    const changed = structuredClone(state)
    changed.digest = '0000000000000001'
    const r1 = changed.routes[1]
    if (r1) r1.reason = 'hostname is held by qemu/101 since a minute'
    replies.set('GET /api/v1/state', ok(changed, changed.digest))
    s.notice({ kind: 'state', data: { at: 'x', finishedAt: 'y', digest: changed.digest } })
    await flush()
    const after = s.get().state?.routes ?? []
    expect(after[0]).toBe(first)
    expect(after[1]).not.toBe(second)
    expect(after[1]?.reason).toBe('hostname is held by qemu/101 since a minute')
  })

  test('the state is fetched whole every 5 minutes, as its digest has no times', async () => {
    const s = await opened()
    calls = []
    pass(fullFetchEvery - 1000)
    s.tick()
    expect(calls).toEqual([])
    pass(1000)
    s.tick()
    await flush()
    expect(calls).toHaveLength(1)
    expect(calls[0]?.opts?.ifNoneMatch).toBeUndefined()
  })
})

describe('events, gaps, traffic, reset', () => {
  const ev = (seq: number): Event => ({ ...(events[0] as Event), seq })

  test('events are appended once each, the last 500', async () => {
    const s = await opened()
    s.notice({ kind: 'event', data: ev(10) })
    for (let seq = 11; seq < 611; seq++) s.notice({ kind: 'event', data: ev(seq) }, `${hello.boot}:${seq}`)
    const seqs = s.get().events.map((e) => e.seq)
    expect(seqs).toHaveLength(500)
    expect(seqs[0]).toBe(111)
    expect(seqs.at(-1)).toBe(610)
  })

  test('a gap keeps a marker the events table can load', async () => {
    const s = await opened()
    const gap = { boot: hello.boot, from: 814, to: 1826, count: 1013, level: 'error' }
    s.notice({ kind: 'gap', data: gap })
    s.notice({ kind: 'gap', data: gap })
    expect(s.get().gaps).toEqual([gap])
  })

  test('reset drops what the page holds and loads it again', async () => {
    const s = await opened()
    s.notice({ kind: 'gap', data: { boot: hello.boot, from: 1, to: 2, count: 2, level: 'info' } })
    calls = []
    s.notice({ kind: 'reset', data: { reason: 'boot changed' } })
    expect(s.get().state).toBeUndefined()
    expect(s.get().events).toEqual([])
    expect(s.get().gaps).toEqual([])
    expect(s.get().traffic).toBeUndefined()
    await flush()
    expect(s.get().state?.digest).toBe(populated.digest)
    expect(calls.find((c) => c.path === '/api/v1/state')?.opts?.ifNoneMatch).toBeUndefined()
  })

  // The web process lost a traffic notice, perhaps the one that told of a route
  // that stopped, which no later notice tells again.
  test('a reset for a lost traffic notice reads the traffic again, and drops nothing else', async () => {
    const s = await opened()
    expect(s.get().traffic?.routes).toHaveLength(1)
    const { state: held, events: heldEvents } = s.get()
    calls = []
    replies.set('GET /api/v1/traffic', ok({ ...traffic, routes: [], routesTotal: 0 }))
    s.notice({ kind: 'reset', data: { reason: resetTraffic } })
    await flush()
    expect(paths()).toEqual(['GET /api/v1/traffic'])
    expect(s.get().traffic?.routes).toEqual([])
    expect(s.get().state).toBe(held)
    expect(s.get().events).toBe(heldEvents)
  })

  test('traffic notices add a sample to tunnels that have a rate', () => {
    const view = traffic as unknown as TrafficView
    const id = view.tunnels[0]?.tunnelId ?? ''
    const n = mergeTraffic(view, {
      at: '2026-10-01T12:00:10Z',
      tunnels: [
        { tunnelId: id, rps: 40, errorsPerSec: 0.2, concurrent: 4, haConnections: 4, stale: false, sampled: true },
        // a tunnel scraped once: no rate yet
        { tunnelId: 'new', rps: 0, errorsPerSec: 0, concurrent: 0, haConnections: 2, stale: false },
      ],
      routes: [{ hostname: 'www.example.com', owner: 'qemu/101', target: '10.0.0.11:8080', flowsPerSec: 3.1, stale: false }],
      routesTotal: 1,
    })
    expect(n.tunnels[0]?.samples).toHaveLength(180)
    expect(n.tunnels[0]?.samples.at(-1)).toEqual({ at: '2026-10-01T12:00:10Z', rps: 40, errorsPerSec: 0.2, concurrent: 4 })
    expect(n.tunnels[1]).toMatchObject({ tunnelId: 'new', haConnections: 2, samples: [] })
    expect(n.routes).toEqual([{ hostname: 'www.example.com', owner: 'qemu/101', target: '10.0.0.11:8080', flowsPerSec: 3.1, stale: false }])

    const why = mergeTraffic(n, { at: 'z', tunnels: [], routesTotal: 0, routesWhy: 'the egress filter was not checked yet' })
    expect(why.routes).toEqual([])
    expect(why.routesWhy).toBe('the egress filter was not checked yet')
    expect(why.tunnels).toEqual([])
  })
})

describe('the connection', () => {
  test('upstream {up:false} is daemon-down, with its time, until it is up again', async () => {
    const s = await opened()
    s.notice({ kind: 'upstream', data: { up: false, since: '2026-10-01T12:01:30Z' } })
    expect(s.get().conn).toBe('daemon-down')
    expect(s.get().connSince).toBe('2026-10-01T12:01:30Z')
    s.notice({ kind: 'upstream', data: { up: true, since: '2026-10-01T12:02:00Z' } })
    expect(s.get().conn).toBe('live')
  })

  test('stale after 3 × max(poll, cycle): a 40 s cycle with a 10 s poll is still live at 100 s', async () => {
    const s = await opened()
    s.notice({ kind: 'state', data: { at: '2026-10-01T12:00:00Z', finishedAt: '2026-10-01T12:00:40Z', digest: populated.digest } })
    expect(staleAfterMs(s.get())).toBe(120_000)
    pass(100_000)
    s.tick()
    expect(s.get().conn).toBe('live')
    expect(dataAge(s.get(), mono)).toBe(100_000)
    pass(20_001)
    s.tick()
    expect(s.get().conn).toBe('stale')
    // the time shown is the node's
    expect(s.get().connSince).toBe('2026-10-01T12:00:40Z')
  })

  test('a 2 s cycle with a 10 s poll is stale after 30 s', async () => {
    const s = await opened()
    s.notice({ kind: 'state', data: { at: '2026-10-01T12:00:00Z', finishedAt: '2026-10-01T12:00:02Z', digest: populated.digest } })
    expect(staleAfterMs(s.get())).toBe(30_000)
  })

  test('reconnecting is stale after 15 s; the web process away is web-down', async () => {
    const s = await opened()
    s.link({ state: 'reconnecting', since: '2026-10-01T12:00:05Z' })
    expect(s.get().conn).toBe('reconnecting')
    pass(15_001)
    s.tick()
    expect(s.get().conn).toBe('stale')
    s.link({ state: 'web-down', since: '2026-10-01T12:00:05Z' })
    expect(s.get()).toMatchObject({ conn: 'web-down', connSince: '2026-10-01T12:00:05Z' })
  })

  test('the same cycle fetched again is not news; a later one is', async () => {
    const s = await opened()
    const first = s.get().receivedAt
    expect(first).toBe(mono)
    pass(20_000)
    await s.fetchState(false)
    expect(s.get().receivedAt).toBe(first)
    replies.set('GET /api/v1/state', ok({ ...state, finishedAt: '2026-10-01T12:00:12Z' }, state.digest))
    await s.fetchState(false)
    expect(s.get().receivedAt).toBe(mono)
  })

  test("a notice another tab kept is as old as it was there", async () => {
    const s = await opened()
    s.notice({ kind: 'state', data: { at: 'a', finishedAt: '2026-10-01T12:00:12Z', digest: populated.digest } }, '', 25_000)
    expect(dataAge(s.get(), mono)).toBe(25_000)
  })
})

// The browser's clock 31 s ahead of the node's, and 31 s behind: the age of
// the data and the end of the session are measured on the browser's clock.
describe.each([31_000, -31_000])("a browser %i ms off the node's clock", (skew) => {
  const node = Date.parse('2026-10-01T12:00:05Z')

  test('live while the news is fresh, stale 30 s after the last', async () => {
    now = node + skew
    const s = await opened()
    s.notice({ kind: 'state', data: { at: '2026-10-01T12:00:03Z', finishedAt: '2026-10-01T12:00:05Z', digest: populated.digest } })
    pass(29_000)
    s.tick()
    expect(s.get().conn).toBe('live')
    pass(2000)
    s.tick()
    expect(s.get().conn).toBe('stale')
  })

  test('the session idles out 30 min after the last activity of any tab, on the browser\'s clock', async () => {
    now = node + skew
    const at = (t: number) => new Date(t).toISOString()
    replies.set('GET /api/session', () => ({
      status: 200,
      body: { ...session, idleExpiresAt: at(node + 30 * 60_000), expiresAt: at(node + 12 * 3_600_000) },
      date: new Date(node).toUTCString(),
    }))
    const s = await opened()
    expect(s.idleDeadline()).toBe(node + skew + 30 * 60_000)
    // another tab was active 10 minutes later
    pass(10 * 60_000)
    replies.set('GET /api/session', () => ({
      status: 200,
      body: { ...session, idleExpiresAt: at(node + 40 * 60_000), expiresAt: at(node + 12 * 3_600_000) },
      date: new Date(node + 10 * 60_000).toUTCString(),
    }))
    await s.refreshSession()
    expect(s.idleDeadline()).toBe(node + skew + 40 * 60_000)
  })
})

describe('sign-in and sign-out', () => {
  test('with a ticket the page signs in by itself', async () => {
    replies.set('GET /api/session', () => new ApiError(401, unauthenticated))
    replies.set('POST /api/session/ticket', ok(session))
    const s = store()
    await s.loadSession()
    expect(paths()).toEqual(['GET /api/session', 'POST /api/session/ticket'])
    expect(s.get().auth).toBe('signed-in')
  })

  test('after a sign-out it does not, while the flag of the tab is set', async () => {
    replies.set('DELETE /api/session', () => ({ status: 204, body: undefined }))
    const s = await opened()
    await s.signOut()
    expect(s.get().auth).toBe('signed-out')
    expect(shared.closed).toBe(1)
    expect(storages.session?.getItem('pco.signedOut')).toBe('1')

    replies.set('GET /api/session', () => new ApiError(401, unauthenticated))
    calls = []
    const again = store()
    await again.loadSession()
    expect(paths()).toEqual(['GET /api/session'])
    expect(again.get()).toMatchObject({ auth: 'signed-out', unauthenticated })

    // "Sign in with the Proxmox VE session" clears it
    replies.set('POST /api/session/ticket', ok(session))
    await again.signInTicket()
    expect(storages.session?.getItem('pco.signedOut')).toBeNull()
    expect(again.get().auth).toBe('signed-in')
  })

  test('a 401 over the page with a Proxmox VE session: the ticket is taken at once, and the page keeps what it shows', async () => {
    const s = await opened()
    const before = s.get().state
    replies.set('POST /api/session/ticket', ok(session))
    replies.set('GET /api/v1/state', (c) => (c.opts?.ifNoneMatch === state.digest ? { status: 304, body: undefined } : { status: 200, body: state, etag: state.digest }))
    calls = []
    s.clientHooks().unauthenticated(unauthenticated)
    expect(s.get().signInNeeded).toBe(true)
    await flush()
    expect(paths()).toContain('POST /api/session/ticket')
    expect(s.get().signInNeeded).toBe(false)
    expect(s.get().state).toBe(before)
    expect(shared.resumed).toBe(1)
  })

  test('without one, the dialog says so, and does not try the ticket by itself again', async () => {
    const s = await opened()
    calls = []
    s.clientHooks().unauthenticated({ ...unauthenticated, ticket: false })
    await flush()
    expect(s.get()).toMatchObject({ signInNeeded: true, unauthenticated: { ticket: false } })
    expect(paths()).not.toContain('POST /api/session/ticket')
  })

  test('a 401 in other words, or of the stream: /api/session says how to sign in', async () => {
    const s = await opened()
    replies.set('GET /api/session', () => new ApiError(401, unauthenticated))
    replies.set('POST /api/session/ticket', () => new ApiError(401, { error: 'Proxmox VE did not accept the session of this browser', code: 'ticket_invalid' }))
    calls = []
    s.clientHooks().unauthenticated({ error: 'the ticket expired', code: 'ticket_invalid' })
    await flush()
    await flush()
    expect(paths()).toEqual(['GET /api/session', 'POST /api/session/ticket'])
    expect(s.get()).toMatchObject({ signInNeeded: true, auth: 'signed-in', unauthenticated: { ticket: true } })
    // the stream's 401 now: the ticket was tried once already
    calls = []
    shared.link?.({ state: 'signed-out' })
    await flush()
    await flush()
    expect(paths()).toEqual(['GET /api/session'])
  })

  test('signed in again in another tab: the stream is back, the dialog of this one closes', async () => {
    const s = await opened()
    replies.set('GET /api/session', () => new ApiError(401, { ...unauthenticated, ticket: false }))
    shared.link?.({ state: 'signed-out' })
    await flush()
    expect(s.get().signInNeeded).toBe(true)
    replies.set('GET /api/session', ok(session))
    shared.link?.({ state: 'open', since: '2026-10-01T12:01:00Z' })
    await flush()
    expect(s.get().signInNeeded).toBe(false)
  })

  test('signing in in the dialog resumes the stream', async () => {
    const s = await opened()
    replies.set('GET /api/session', () => new ApiError(401, { ...unauthenticated, ticket: false }))
    shared.link?.({ state: 'signed-out' })
    await flush()
    replies.set('POST /api/session/token', ok(session))
    await s.signInToken('root@pam!pco=00000000-0000-0000-0000-000000000000')
    expect(s.get().signInNeeded).toBe(false)
    expect(shared.resumed).toBe(1)
  })

  test("this browser's last doctor run is kept", () => {
    const s = store()
    s.setDoctorLast({ fail: 2, at: '2026-10-01T12:00:00Z' })
    expect(store().get().doctorLast).toEqual({ fail: 2, at: '2026-10-01T12:00:00Z' })
  })
})

test('shareRoutes keeps the objects whose JSON is equal', () => {
  const a = { hostname: 'a.example.com', owner: 'qemu/1', state: 'active' }
  const b = { hostname: 'b.example.com', owner: 'qemu/2', state: 'active' }
  const out = shareRoutes([a, b], [{ ...a }, { ...b, state: 'held' }, { hostname: 'c.example.com', owner: 'qemu/3', state: 'active' }])
  expect(out[0]).toBe(a)
  expect(out[1]).not.toBe(b)
  expect(out).toHaveLength(3)
})

test('notices of the stream go through notice()', () => {
  const s = store()
  const n: Notice = { kind: 'upstream', data: { up: false, since: '2026-10-01T12:01:30Z' } }
  s.notice(n)
  expect(s.get().upstream).toEqual(n.data)
})

import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { type Link, type Notice, openStream, type StreamHandle } from './stream'

// A fake web process: each connect is answered by the next of the queued
// answers, or by the network error of a process that is away.
interface Connect {
  at: number
  lastEventId?: string
}

let connects: Connect[]
let probes: number
let answers: (() => Response | Error)[]
let sessionUp: boolean
let handle: StreamHandle | undefined

function sse(text: string, open = false): () => Response {
  return () =>
    new Response(
      new ReadableStream<Uint8Array>({
        start(c) {
          c.enqueue(new TextEncoder().encode(text))
          if (!open) c.close()
        },
      }),
      { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
    )
}

const status = (code: number, body: unknown = {}) => () => new Response(JSON.stringify(body), { status: code })
const away = () => new TypeError('Failed to fetch')

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-05T12:00:00Z'))
  connects = []
  probes = 0
  answers = []
  sessionUp = true
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      if (url === '/api/session') {
        probes++
        if (!sessionUp) throw new TypeError('Failed to fetch')
        return new Response('{}', { status: 200 })
      }
      const h = (init.headers ?? {}) as Record<string, string>
      connects.push({ at: Date.now(), lastEventId: h['Last-Event-ID'] })
      const next = answers.shift() ?? away
      const got = next()
      if (got instanceof Error) throw got
      return got
    }),
  )
})

afterEach(() => {
  handle?.close()
  handle = undefined
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

function start(lastEventId = '') {
  const notices: Notice[] = []
  const links: Link[] = []
  handle = openStream({ onNotice: (n) => notices.push(n), onLink: (l) => links.push(l), lastEventId: () => lastEventId })
  return { notices, links }
}

const t0 = new Date('2026-10-05T12:00:00Z').getTime()
const offsets = () => connects.map((c) => c.at - t0)

describe('back-off', () => {
  test('1, 2, 5, 10 s, then 10 s each, while the stream answers 503', async () => {
    answers = Array.from({ length: 7 }, () => status(503, { code: 'unavailable', error: 'x' }))
    const { links } = start()
    await vi.advanceTimersByTimeAsync(40_000)
    expect(offsets()).toEqual([0, 1000, 3000, 8000, 18_000, 28_000, 38_000])
    expect(links.at(-1)).toEqual({ state: 'reconnecting', since: '2026-10-05T12:00:00.000Z' })
  })

  test('a stream that ends right after its hello does not start the back-off over; the next connect resumes after the last event', async () => {
    answers = [
      status(502),
      status(502),
      sse('event: hello\ndata: {"boot":"b","version":"1","seq":3,"digest":"d","pollInterval":"10s"}\n\nid: b:4\nevent: event\ndata: {"seq":4}\n\n'),
      status(502),
    ]
    const { notices, links } = start('b:2')
    await vi.advanceTimersByTimeAsync(10_000)
    // 0, 1 s, 3 s (hello; the stream ends at once), 8 s
    expect(offsets()).toEqual([0, 1000, 3000, 8000])
    expect(connects.map((c) => c.lastEventId)).toEqual(['b:2', 'b:2', 'b:2', 'b:4'])
    expect(notices.map((n) => n.kind)).toEqual(['hello', 'event'])
    expect(links.map((l) => l.state)).toEqual(['connecting', 'reconnecting', 'reconnecting', 'open', 'reconnecting', 'reconnecting'])
  })

  test('a stream that stayed open 30 s starts it over', async () => {
    let end: (() => void) | undefined
    answers = [
      status(502),
      status(502),
      () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(c) {
              c.enqueue(new TextEncoder().encode('event: hello\ndata: {}\n\n'))
              end = () => c.close()
            },
          }),
          { status: 200 },
        ),
      status(502),
    ]
    start()
    await vi.advanceTimersByTimeAsync(3000 + 30_000)
    end?.()
    await vi.advanceTimersByTimeAsync(1000)
    expect(offsets()).toEqual([0, 1000, 3000, 34_000])
  })
})

test('a notice the page fails to take is reported, not taken for a broken connection', async () => {
  const error = vi.spyOn(console, 'error').mockImplementation(() => {})
  answers = [sse('event: state\ndata: {}\n\nevent: upstream\ndata: {"up":true,"since":""}\n\n', true)]
  const kinds: string[] = []
  const links: Link[] = []
  handle = openStream({
    onNotice: (n) => {
      kinds.push(n.kind)
      if (n.kind === 'state') throw new TypeError('a bug of the page')
    },
    onLink: (l) => links.push(l),
  })
  await vi.advanceTimersByTimeAsync(60_000)
  expect(kinds).toEqual(['state', 'upstream'])
  expect(connects).toHaveLength(1)
  expect(links.map((l) => l.state)).toEqual(['connecting'])
  expect(error).toHaveBeenCalledTimes(1)
  error.mockRestore()
})

test('401: the sign-in dialog, and no attempt until the user signed in again', async () => {
  answers = [status(401, { code: 'unauthenticated', methods: ['ticket'], ticket: true }), sse('event: hello\ndata: {}\n\n', true)]
  const { links } = start()
  await vi.advanceTimersByTimeAsync(60_000)
  expect(connects).toHaveLength(1)
  expect(links.at(-1)).toEqual({ state: 'signed-out' })
  handle?.resume()
  await vi.advanceTimersByTimeAsync(0)
  expect(connects).toHaveLength(2)
  expect(links.at(-1)?.state).toBe('open')
})

test('429: too many tabs, and the next attempt after retryAfter', async () => {
  answers = [status(429, { code: 'rate_limited', error: 'too many streams', retryAfter: 7 }), status(429, { code: 'rate_limited' })]
  const { links } = start()
  await vi.advanceTimersByTimeAsync(7000)
  expect(offsets()).toEqual([0, 7000])
  expect(links[1]).toEqual({ state: 'too-many', since: '2026-10-05T12:00:00.000Z', retryAfter: 7 })
  // without retryAfter, 30 s
  await vi.advanceTimersByTimeAsync(29_999)
  expect(connects).toHaveLength(2)
  await vi.advanceTimersByTimeAsync(1)
  expect(connects).toHaveLength(3)
})

test('a failed connect is the web process away when /api/session fails too, with its time', async () => {
  sessionUp = false
  answers = [away, away]
  const { links } = start()
  await vi.advanceTimersByTimeAsync(0)
  expect(probes).toBe(1)
  expect(links.at(-1)).toEqual({ state: 'web-down', since: '2026-10-05T12:00:00.000Z' })
  sessionUp = true
  await vi.advanceTimersByTimeAsync(1000)
  // the session answers, only the stream does not: reconnecting, since the first loss
  expect(links.at(-1)).toEqual({ state: 'reconnecting', since: '2026-10-05T12:00:00.000Z' })
})

test('unknown notices and bad JSON are passed over; close ends it', async () => {
  answers = [sse('event: novel\ndata: {}\n\nevent: state\ndata: {not json\n\nevent: upstream\ndata: {"up":false,"since":"2026-10-05T11:59:00Z"}\n\n', true)]
  const { notices } = start()
  await vi.advanceTimersByTimeAsync(0)
  expect(notices).toEqual([{ kind: 'upstream', data: { up: false, since: '2026-10-05T11:59:00Z' } }])
  handle?.close()
  await vi.advanceTimersByTimeAsync(60_000)
  expect(connects).toHaveLength(1)
})

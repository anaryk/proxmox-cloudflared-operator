// The stream of pco web, read with fetch so that its
// status is seen: a 401 opens the sign-in dialog, a 429 says why, and the
// back-off is the page's own.

import { readSse } from './sse'
import type { Event, GapNotice, Hello, StateNotice, TrafficNotice, Upstream } from './types.gen'

export const streamPath = '/api/v1/stream'

export type Notice =
  | { kind: 'hello'; data: Hello }
  | { kind: 'state'; data: StateNotice }
  | { kind: 'event'; data: Event }
  | { kind: 'gap'; data: GapNotice }
  | { kind: 'traffic'; data: TrafficNotice }
  | { kind: 'reset'; data: { reason: string } }
  | { kind: 'upstream'; data: Upstream }

const kinds: ReadonlySet<string> = new Set(['hello', 'state', 'event', 'gap', 'traffic', 'reset', 'upstream'])

// The state of the connection, with since when for those that last.
export type Link =
  | { state: 'connecting' }
  | { state: 'open'; since: string }
  | { state: 'reconnecting'; since: string }
  | { state: 'web-down'; since: string }
  | { state: 'too-many'; since: string; retryAfter: number }
  | { state: 'signed-out' }

export const tooManyText = 'Too many pco tabs are open in this browser session'

// The waits between attempts, in milliseconds: then 10 s each.
export const backoff: readonly number[] = [1000, 2000, 5000, 10_000]

// A stream that stayed open this long starts the back-off over when it
// breaks; one that breaks sooner, such as right after its hello, goes on
// with it.
export const steadyAfter = 30_000

export interface StreamOptions {
  onNotice: (n: Notice, id: string) => void
  onLink: (l: Link) => void
  // The id of the last event this tab holds, to resume after it.
  lastEventId?: () => string
}

export interface StreamHandle {
  close: () => void
  // Opens the stream again after a sign-in.
  resume: () => void
}

function parse(event: string, data: string): Notice | undefined {
  if (!kinds.has(event)) return undefined
  try {
    return { kind: event, data: JSON.parse(data) } as Notice
  } catch {
    return undefined
  }
}

const headers = (background: boolean, id: string): Record<string, string> => {
  const h: Record<string, string> = { Accept: background ? 'application/json' : 'text/event-stream', 'Pco-Background': '1' }
  if (id) h['Last-Event-ID'] = id
  return h
}

export function openStream(opts: StreamOptions): StreamHandle {
  const stop = new AbortController()
  let wake: (() => void) | undefined
  let lastId = opts.lastEventId?.() ?? ''
  let attempt = 0
  let since: string | undefined // when the connection was lost

  const sleep = (ms: number) =>
    new Promise<void>((resolve) => {
      const timer = setTimeout(resolve, ms)
      stop.signal.addEventListener('abort', () => {
        clearTimeout(timer)
        resolve()
      })
    })

  const signedOut = () =>
    new Promise<void>((resolve) => {
      wake = resolve
      stop.signal.addEventListener('abort', () => resolve())
    })

  // A failed connect: is the web process away, or only the stream?
  const reachable = async () => {
    try {
      await fetch('/api/session', { headers: headers(true, ''), credentials: 'same-origin', cache: 'no-store', signal: stop.signal })
      return true
    } catch {
      return false
    }
  }

  // A notice the page fails to take is a bug of the page, not a break of the
  // connection: it is reported, and the stream goes on.
  const deliver = (n: Notice, id: string) => {
    try {
      opts.onNotice(n, id)
    } catch (err) {
      console.error(`pco: the page failed to take a ${n.kind} notice of the stream`, err)
    }
  }

  const lost = (state: 'reconnecting' | 'web-down') => {
    since ??= new Date().toISOString()
    opts.onLink({ state, since })
  }

  const run = async () => {
    opts.onLink({ state: 'connecting' })
    while (!stop.signal.aborted) {
      let res: Response
      try {
        res = await fetch(streamPath, { headers: headers(false, lastId), credentials: 'same-origin', cache: 'no-store', signal: stop.signal })
      } catch {
        if (stop.signal.aborted) return
        lost((await reachable()) ? 'reconnecting' : 'web-down')
        await sleep(backoff[Math.min(attempt++, backoff.length - 1)] ?? 10_000)
        continue
      }

      if (res.status === 401) {
        void res.body?.cancel()
        opts.onLink({ state: 'signed-out' })
        await signedOut()
        attempt = 0
        continue
      }
      if (res.status === 429) {
        let after = 30
        try {
          const body = (await res.json()) as { retryAfter?: number }
          if (body.retryAfter && body.retryAfter > 0) after = body.retryAfter
        } catch {
          // the default
        }
        since ??= new Date().toISOString()
        opts.onLink({ state: 'too-many', since, retryAfter: after })
        await sleep(after * 1000)
        continue
      }
      if (!res.ok || !res.body) {
        void res.body?.cancel()
        lost('reconnecting')
        await sleep(backoff[Math.min(attempt++, backoff.length - 1)] ?? 10_000)
        continue
      }

      let opened: number | undefined
      try {
        for await (const msg of readSse(res.body, lastId)) {
          lastId = msg.id
          const n = parse(msg.event, msg.data)
          if (!n) continue
          if (n.kind === 'hello') {
            opened = Date.now()
            since = undefined
            opts.onLink({ state: 'open', since: new Date().toISOString() })
          }
          deliver(n, msg.id)
        }
      } catch {
        // the connection broke; it is opened again below
      }
      if (stop.signal.aborted) return
      if (opened !== undefined && Date.now() - opened >= steadyAfter) attempt = 0
      lost('reconnecting')
      await sleep(backoff[Math.min(attempt++, backoff.length - 1)] ?? 10_000)
    }
  }
  void run()

  return {
    close: () => stop.abort(),
    resume: () => {
      wake?.()
      wake = undefined
    },
  }
}

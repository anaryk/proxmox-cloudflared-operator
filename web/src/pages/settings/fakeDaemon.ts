// A stand-in for the daemon behind fetch, for the tests of this page: each
// route of it answers a call by "METHOD /path", and every call is kept in
// order.

import { vi } from 'vitest'

export interface Sent {
  method: string
  path: string
  query: string
  body?: unknown
}

export interface Reply {
  status?: number
  body?: unknown
}

export type Handler = (sent: Sent) => Reply | undefined

export interface FakeDaemon {
  calls: Sent[]
  // The calls that write, as "METHOD path".
  writes: () => string[]
}

export function fakeDaemon(handlers: Record<string, Handler>): FakeDaemon {
  const calls: Sent[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const [path = '', query = ''] = url.split('?')
      const sent: Sent = { method: init.method ?? 'GET', path, query, body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined }
      calls.push(sent)
      const reply = handlers[`${sent.method} ${path}`]?.(sent) ?? { status: 404, body: { code: 'not_found', error: 'no answer in the test' } }
      return new Response(JSON.stringify(reply.body ?? {}), { status: reply.status ?? 200 })
    }),
  )
  return { calls, writes: () => calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${c.path}${c.query ? `?${c.query}` : ''}`) }
}

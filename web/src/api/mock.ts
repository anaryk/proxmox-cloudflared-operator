// The answers of pco web for the development server, from the fixtures, so
// that pages can be built without a node: VITE_MOCK=1 npm run dev, and
// VITE_MOCK_STATE=<fixture> (populated, empty, untagged, tagged-empty,
// rogue, frozen, rogue-scenario, large) for another state. Nothing changes:
// a write is refused as a daemon that is busy would refuse it. It is used by
// vite.config.ts only and is never part of the build.

import { readFileSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'

import type { Plugin } from 'vite'

import { largeState, trafficOf } from '../../scripts/fixtures.mjs'

// The development server loads this file with its configuration, in Node,
// which reads the fixtures as files: nothing here needs the resolution the
// page's own build does.
const fixture = (name: string): unknown => JSON.parse(readFileSync(new URL(`../fixtures/${name}.json`, import.meta.url), 'utf8'))

interface MockState {
  digest?: string
  credentials: unknown[]
}

interface MockTraffic {
  tunnels: { tunnelId: string; haConnections: number }[]
  routes: object[]
  routesTotal: number
}

interface MockEvent {
  seq: number
}

const states = ['populated', 'empty', 'untagged', 'tagged-empty', 'rogue', 'frozen', 'rogue-scenario']

export interface MockAnswer {
  status: number
  headers?: Record<string, string>
  body?: unknown
}

const notFound = (error: string): MockAnswer => ({ status: 404, body: { error, code: 'not_found' } })

// Mock is pco web as the fixtures have it, signed in as the admin of the
// session fixture until the page signs out.
export class Mock {
  state: MockState
  traffic: MockTraffic
  events = fixture('events') as MockEvent[]
  hello = fixture('hello') as { boot: string; seq: number }
  signedOut = false

  constructor(name = 'populated') {
    this.state = (name === 'large' ? largeState(1000) : fixture(states.includes(name) ? name : 'populated')) as MockState
    this.traffic = trafficOf(this.state, '2026-10-01T12:00:05Z') as MockTraffic
  }

  session(now: Date, method = 'ticket') {
    return {
      ...(fixture('session') as object),
      method,
      idleExpiresAt: new Date(now.getTime() + 30 * 60_000).toISOString(),
      expiresAt: new Date(now.getTime() + 12 * 3_600_000).toISOString(),
    }
  }

  answer(method: string, url: string, headers: Record<string, string | string[] | undefined>, now = new Date()): MockAnswer {
    const { pathname, searchParams } = new URL(url, 'https://pve1.example.lan:8643')
    switch (`${method} ${pathname}`) {
      case 'GET /api/session':
        return this.signedOut ? { status: 401, body: fixture('unauthenticated') } : { status: 200, body: this.session(now) }
      case 'POST /api/session/ticket':
        this.signedOut = false
        return { status: 200, body: this.session(now) }
      case 'POST /api/session/token':
        this.signedOut = false
        return { status: 200, body: this.session(now, 'token') }
      case 'DELETE /api/session':
        this.signedOut = true
        return { status: 204 }
      case 'POST /api/session/touch':
        return { status: 204 }
    }
    if (this.signedOut && pathname.startsWith('/api/v1/')) return { status: 401, body: fixture('unauthenticated') }
    // the doctor is a POST that changes nothing
    if (method === 'POST' && pathname === '/api/v1/doctor') return { status: 200, body: fixture('doctor') }
    if (method !== 'GET') {
      return { status: 503, body: { error: 'the development server changes nothing: try again on a node', code: 'unavailable' } }
    }
    switch (pathname) {
      case '/api/v1/state': {
        const tag = `"${this.state.digest ?? ''}"`
        if (headers['if-none-match'] === tag) return { status: 304, headers: { ETag: tag } }
        return { status: 200, headers: { ETag: tag }, body: this.state }
      }
      case '/api/v1/events': {
        const after = Number(searchParams.get('after') ?? 0)
        return { status: 200, body: this.events.filter((e) => e.seq > after) }
      }
      case '/api/v1/traffic':
        return { status: 200, body: this.traffic }
      case '/api/v1/credentials':
        return { status: 200, body: this.state.credentials }
    }
    const named: Record<string, string> = {
      '/api/v1/version': 'version',
      '/api/v1/settings': 'settings',
      '/api/v1/guests': 'guests',
      '/api/v1/routes/manual': 'manual-routes',
      '/api/v1/claims': 'claims',
      '/api/v1/approvals': 'approvals',
    }
    const name = Object.hasOwn(named, pathname) ? named[pathname] : undefined
    if (name) return { status: 200, body: fixture(name) }
    if (/^\/api\/v1\/guests\/(qemu|lxc)\/\d+\/annotation$/.test(pathname)) return { status: 200, body: fixture('annotation') }
    return notFound(`the development server has no answer for ${pathname}`)
  }

  // stream writes the notices of a stream until the request ends: a state
  // notice every poll, traffic every 5 s, an event now and then.
  stream(req: IncomingMessage, res: ServerResponse): void {
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
    const send = (event: string, data: unknown, id?: string) => res.write(`${id ? `id: ${id}\n` : ''}event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)
    const digest = this.state.digest ?? ''
    send('hello', { ...this.hello, digest })
    send('upstream', { up: true, since: new Date().toISOString() })
    let seq = this.hello.seq
    const cycle = () => {
      const at = new Date()
      send('state', { at: at.toISOString(), finishedAt: new Date(at.getTime() + 1800).toISOString(), digest })
    }
    cycle()
    const timers = [
      setInterval(cycle, 10_000),
      setInterval(() => {
        send('traffic', {
          at: new Date().toISOString(),
          tunnels: this.traffic.tunnels.map((t) => ({
            tunnelId: t.tunnelId,
            rps: Math.round((30 + 10 * Math.random()) * 10) / 10,
            errorsPerSec: 0.1,
            concurrent: 3,
            haConnections: t.haConnections,
            stale: false,
            sampled: true,
          })),
          routes: this.traffic.routes.map((r) => ({ ...r, flowsPerSec: Math.round(Math.random() * 40) / 10 })),
          routesTotal: this.traffic.routesTotal,
        })
      }, 5000),
      setInterval(() => {
        seq++
        const ev = this.events[seq % this.events.length]
        send('event', { ...ev, seq, at: new Date().toISOString() }, `${this.hello.boot}:${seq}`)
      }, 20_000),
      setInterval(() => res.write(': ping\n\n'), 15_000),
    ]
    req.on('close', () => {
      for (const t of timers) clearInterval(t)
    })
  }
}

// mockApi is the plugin of the development server that answers /api.
export function mockApi(name?: string): Plugin {
  const mock = new Mock(name)
  return {
    name: 'pco-mock',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = req.url ?? ''
        if (!url.startsWith('/api/')) {
          next()
          return
        }
        if (req.method === 'GET' && url.startsWith('/api/v1/stream') && !mock.signedOut) {
          mock.stream(req, res)
          return
        }
        // The body of a write is not read: nothing changes.
        req.resume()
        const a = mock.answer(req.method ?? 'GET', url, req.headers)
        res.writeHead(a.status, { 'Content-Type': 'application/json', ...a.headers })
        res.end(a.body === undefined ? undefined : JSON.stringify(a.body))
      })
    },
  }
}

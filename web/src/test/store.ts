// A store for the tests of the page, fed from the fixtures as pco web would
// feed it, without a network.

import events from '../fixtures/events.json'
import hello from '../fixtures/hello.json'
import session from '../fixtures/session.json'
import settings from '../fixtures/settings.json'
import { type Answer, ApiError, type Method, type RequestOptions } from '../api/client'
import { type AppState, AppStore } from '../api/store'
import type { Event, Hello, Session, State } from '../api/types.gen'

export interface Call {
  method: Method
  path: string
  body?: unknown
  opts?: RequestOptions
}

export interface FakeOptions {
  state: unknown
  events?: Event[]
  session?: Partial<Session>
  hello?: Partial<Hello>
  // more answers, by "METHOD /path"
  answers?: Record<string, (c: Call) => Answer | ApiError>
}

export interface Fake {
  store: AppStore
  calls: Call[]
}

export const flush = () => new Promise((r) => setTimeout(r, 0))

export function memoryStorage(): Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> {
  const m = new Map<string, string>()
  return {
    getItem: (k) => m.get(k) ?? null,
    setItem: (k, v) => void m.set(k, v),
    removeItem: (k) => void m.delete(k),
  }
}

// fakeStore is a store signed in with the session fixture, after the hello of
// its stream and the first fetch of the state.
export async function fakeStore(o: FakeOptions): Promise<Fake> {
  const calls: Call[] = []
  const st = o.state as State
  const answers: Record<string, (c: Call) => Answer | ApiError> = {
    'GET /api/session': () => ({ status: 200, body: { ...session, ...o.session } }),
    'GET /api/v1/state': () => ({ status: 200, body: st, etag: st.digest }),
    'GET /api/v1/events': () => ({ status: 200, body: o.events ?? events }),
    'GET /api/v1/traffic': () => new ApiError(404, { code: 'not_found', error: 'none' }),
    'GET /api/v1/settings': () => ({ status: 200, body: settings }),
    ...o.answers,
  }
  const store = new AppStore({
    now: () => Date.now(),
    storages: { session: memoryStorage(), local: memoryStorage() },
    request: async <T>(method: Method, path: string, body?: unknown, opts?: RequestOptions) => {
      const c = { method, path, body, opts }
      calls.push(c)
      const reply = answers[`${method} ${path.split('?')[0]}`]
      const got = reply ? reply(c) : new ApiError(404, { code: 'not_found', error: 'no answer in the test' })
      if (got instanceof ApiError) throw got
      return got as Answer<T>
    },
    share: () => ({ close: () => {}, resume: () => {}, leading: () => true }),
  })
  await store.loadSession()
  store.link({ state: 'open', since: new Date().toISOString() })
  store.notice({ kind: 'hello', data: { ...hello, digest: st.digest ?? '', ...o.hello } })
  await flush()
  return { store, calls }
}

// appState is an AppState for the pure functions of the shell.
export function appState(patch: Partial<AppState> = {}): AppState {
  return {
    events: [],
    gaps: [],
    conn: 'live',
    upstream: { up: true, since: '' },
    loadedVersion: '',
    auth: 'signed-in',
    signInNeeded: false,
    link: { state: 'open', since: '' },
    skew: false,
    ...patch,
  }
}

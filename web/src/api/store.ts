// What the page knows: the session, the daemon's state and traffic, the
// latest events and how live all of it is. The stream says what changed; the
// state itself is fetched only when its digest changes.

import { createContext, createElement, type ReactNode, useContext, useRef, useSyncExternalStore } from 'react'

import { parseDuration } from '../text/duration'
import { ApiError, type Answer, type ClientHooks, type Method, request as defaultRequest, type RequestOptions, setClientHooks } from './client'
import { channelName, type Locks, shareStream, type Shared } from './leader'
import { type Link, type Notice, openStream, resetTraffic } from './stream'
import type {
  Event,
  GapNotice,
  Hello,
  RouteTraffic,
  RouteView,
  Session,
  SettingsView,
  State,
  StateNotice,
  TrafficNotice,
  TrafficView,
  TunnelTraffic,
  Unauthenticated,
  Upstream,
} from './types.gen'

export type Conn = 'live' | 'reconnecting' | 'stale' | 'daemon-down' | 'web-down'

export interface AppState {
  session?: Session
  hello?: Hello
  state?: State // routes shared by (hostname, owner) across updates
  times?: StateNotice // at, finishedAt, digest of the last notice
  // When the last cycle's news reached this browser, on its monotonic
  // clock (monoNow): the age of the data is measured from it, as the
  // node's clock and the browser's need not agree.
  receivedAt?: number
  traffic?: TrafficView
  events: Event[] // the last 500
  gaps: GapNotice[]
  conn: Conn
  connSince?: string
  upstream: Upstream
  loadedVersion: string // the daemon version the page started with
  doctorLast?: { fail: number; at: string } // this browser's last run
  settings?: SettingsView

  // Whether the user is signed in, and if not, how they may sign in.
  auth: 'loading' | 'signed-in' | 'signed-out'
  unauthenticated?: Unauthenticated
  authError?: ApiError
  // A call of the API found the session gone: the dialog over the page.
  signInNeeded: boolean
  link: Link
  // The page is older than the daemon: another version answered, a route
  // the page knows is gone, or a part of the page did not load.
  skew: boolean
}

export const maxEvents = 500
const maxGaps = 100
const maxSamples = 180
// The state is fetched whole at least this often: its digest leaves out
// every time, and so the times of the proofs in it.
export const fullFetchEvery = 5 * 60_000
// Reconnecting longer than this is stale.
const reconnectingStale = 15_000
const defaultPoll = 10_000

const signedOutKey = 'pco.signedOut'
const doctorKey = 'pco.doctor'

const zeroTime = (at?: string) => !at || at.startsWith('0001-01-01T00:00:00')

// staleAfterMs is 3 × max(pollInterval, finishedAt - at): the
// base of the doctor's cycle check, so the page and pco doctor agree.
export function staleAfterMs(s: AppState): number {
  const poll = parseDuration(s.hello?.pollInterval ?? '') ?? defaultPoll
  const at = s.times?.at ?? s.state?.at
  const finished = s.times?.finishedAt ?? s.state?.finishedAt
  const cycle = !zeroTime(at) && !zeroTime(finished) ? Date.parse(finished ?? '') - Date.parse(at ?? '') : 0
  return 3 * Math.max(poll, Number.isFinite(cycle) ? cycle : 0)
}

// lastCycle is when the last cycle finished, if one has, on the node's
// clock: a time to show, not one to subtract from the browser's.
export function lastCycle(s: AppState): string | undefined {
  const finished = s.times?.finishedAt ?? s.state?.finishedAt
  return zeroTime(finished) ? undefined : finished
}

// monoNow is the browser's monotonic clock, which no change of the system
// time moves.
export const monoNow = (): number => performance.now()

// dataAge is how long ago, in milliseconds, the news of the last cycle
// reached the page; undefined before any.
export function dataAge(s: AppState, mono: number = monoNow()): number | undefined {
  return s.receivedAt === undefined ? undefined : Math.max(mono - s.receivedAt, 0)
}

// connOf says how live the page is. now is the browser's time, for the
// times of the link, which the browser stamped; mono its monotonic clock.
function connOf(s: AppState, now: number, mono: number): { conn: Conn; since?: string } {
  const l = s.link
  if (l.state === 'web-down') return { conn: 'web-down', since: l.since }
  if (!s.upstream.up) return { conn: 'daemon-down', since: s.upstream.since }
  const finished = lastCycle(s)
  if (l.state === 'reconnecting' || l.state === 'too-many') {
    if (now - Date.parse(l.since) > reconnectingStale) return { conn: 'stale', since: finished ?? l.since }
    return { conn: 'reconnecting', since: l.since }
  }
  if (l.state !== 'open') return { conn: 'reconnecting' }
  const age = dataAge(s, mono)
  if (age !== undefined && age > staleAfterMs(s)) return { conn: 'stale', since: finished }
  return { conn: 'live', since: finished }
}

// shareRoutes keeps the route objects of before whose JSON did not change,
// so that the rows of unchanged routes do not render again.
const jsonOf = new WeakMap<object, string>()

function json(o: object): string {
  let j = jsonOf.get(o)
  if (j === undefined) {
    j = JSON.stringify(o)
    jsonOf.set(o, j)
  }
  return j
}

const routeKey = (r: { hostname: string; owner: string }) => `${r.hostname}\u0000${r.owner}`

export function shareRoutes(before: readonly RouteView[] | undefined, after: RouteView[]): RouteView[] {
  if (!before || before.length === 0) return after
  const old = new Map(before.map((r) => [routeKey(r), r]))
  return after.map((r) => {
    const prev = old.get(routeKey(r))
    return prev && json(prev) === json(r) ? prev : r
  })
}

// mergeTraffic takes a notice of a round of scrapes into the traffic held: a
// sample more for each tunnel that has a rate, the figures of the routes it
// names. A tunnel without "sampled" has no rate yet; one the round left out
// is gone.
export function mergeTraffic(view: TrafficView | undefined, n: TrafficNotice): TrafficView {
  const tunnels = new Map((view?.tunnels ?? []).map((t) => [t.tunnelId, t]))
  const merged: TunnelTraffic[] = n.tunnels.map((t) => {
    const was: TunnelTraffic = tunnels.get(t.tunnelId) ?? {
      tunnelId: t.tunnelId,
      node: '',
      configVersion: 0,
      haConnections: 0,
      edges: [],
      rttMs: [],
      stale: false,
      samples: [],
    }
    const samples = t.sampled ? [...was.samples, { at: n.at, rps: t.rps, errorsPerSec: t.errorsPerSec, concurrent: t.concurrent }].slice(-maxSamples) : was.samples
    return { ...was, haConnections: t.haConnections, stale: t.stale, samples }
  })
  let routes: RouteTraffic[] = []
  if (!n.routesWhy) {
    const byKey = new Map((view?.routes ?? []).map((r) => [routeKey(r), r]))
    for (const r of n.routes ?? []) byKey.set(routeKey(r), r)
    routes = [...byKey.values()].sort((a, b) => (a.hostname < b.hostname ? -1 : a.hostname > b.hostname ? 1 : a.owner < b.owner ? -1 : 1))
  }
  return { at: n.at, interval: view?.interval ?? '5s', tunnels: merged, routes, routesTotal: n.routesTotal, routesWhy: n.routesWhy }
}

function appendEvents(have: Event[], more: readonly Event[]): Event[] {
  if (more.length === 0) return have
  const bySeq = new Map(have.map((e) => [e.seq, e]))
  for (const e of more) bySeq.set(e.seq, e)
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq).slice(-maxEvents)
}

// The storage the store keeps its few things in; either may be refused.
export interface Storages {
  session?: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>
  local?: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>
}

function browserStorages(): Storages {
  const get = (which: 'sessionStorage' | 'localStorage') => {
    try {
      return window[which]
    } catch {
      return undefined
    }
  }
  return { session: get('sessionStorage'), local: get('localStorage') }
}

function readItem(s: Storages[keyof Storages], key: string): string | null {
  try {
    return s?.getItem(key) ?? null
  } catch {
    return null
  }
}

function writeItem(s: Storages[keyof Storages], key: string, value: string | null): void {
  try {
    if (value === null) s?.removeItem(key)
    else s?.setItem(key, value)
  } catch {
    // kept for this page only
  }
}

export interface StoreDeps {
  request?: <T>(method: Method, path: string, body?: unknown, opts?: RequestOptions) => Promise<Answer<T>>
  now?: () => number // the browser's time
  mono?: () => number // its monotonic clock
  storages?: Storages
  // How the stream is shared between tabs; the browser's when not given.
  share?: (o: { onNotice: (n: Notice, id: string, age?: number) => void; onLink: (l: Link) => void; lastEventId: () => string }) => Shared
}

// How long pco web keeps a session without activity.
const idleTimeout = 30 * 60_000

const isUnauthenticated = (body: unknown): body is Unauthenticated =>
  typeof body === 'object' && body !== null && Array.isArray((body as Unauthenticated).methods)

type Listener = () => void

export class AppStore {
  #s: AppState
  #listeners = new Set<Listener>()
  #request: NonNullable<StoreDeps['request']>
  #now: () => number
  #mono: () => number
  #storages: Storages
  #share: NonNullable<StoreDeps['share']>
  #shared?: Shared
  #timers: ReturnType<typeof setInterval>[] = []
  #etag?: string
  #fetching = false
  #fetchAgain = false
  #lastFull = 0
  #lastId = ''
  #lastActive = 0
  // The daemon was found away by a call, not by the stream.
  #downByCall = false
  #started = false
  // The finishedAt receivedAt was stamped for.
  #receivedFor?: string
  // The browser's time less the node's, from the Date of an answer of
  // /api/session.
  #nodeOffset?: number
  // The ticket was tried by itself since the session was lost.
  #ticketTried = false

  constructor(deps: StoreDeps = {}) {
    this.#request = deps.request ?? defaultRequest
    this.#now = deps.now ?? Date.now
    this.#mono = deps.mono ?? monoNow
    this.#storages = deps.storages ?? browserStorages()
    this.#share = deps.share ?? browserShare
    let doctorLast: AppState['doctorLast']
    try {
      const v = JSON.parse(readItem(this.#storages.local, doctorKey) ?? 'null') as AppState['doctorLast'] | null
      if (v && typeof v.fail === 'number' && typeof v.at === 'string') doctorLast = v
    } catch {
      // none kept
    }
    this.#s = {
      events: [],
      gaps: [],
      conn: 'reconnecting',
      upstream: { up: true, since: '' },
      loadedVersion: '',
      doctorLast,
      auth: 'loading',
      signInNeeded: false,
      link: { state: 'connecting' },
      skew: false,
    }
  }

  get = (): AppState => this.#s

  subscribe = (l: Listener): (() => void) => {
    this.#listeners.add(l)
    return () => this.#listeners.delete(l)
  }

  #set(patch: Partial<AppState>): void {
    const next = { ...this.#s, ...patch }
    const c = connOf(next, this.#now(), this.#mono())
    next.conn = c.conn
    next.connSince = c.since
    this.#s = next
    for (const l of this.#listeners) l()
  }

  // clientHooks is what the store learns from every call of the page.
  clientHooks(): ClientHooks {
    return {
      csrf: () => this.#s.session?.csrf,
      unauthenticated: (body) => this.#lost(body),
      versionSkew: () => this.#set({ skew: true }),
      daemonUnreachable: () => {
        if (!this.#s.upstream.up) return
        this.#downByCall = true
        this.#set({ upstream: { up: false, since: new Date(this.#now()).toISOString() } })
      },
      daemonAnswered: () => {
        if (!this.#downByCall) return
        this.#downByCall = false
        this.#set({ upstream: { up: true, since: new Date(this.#now()).toISOString() } })
      },
      active: () => {
        this.#lastActive = this.#now()
      },
    }
  }

  // start signs in as far as the browser can by itself, then follows the
  // stream. It is called once, by the page.
  async start(): Promise<void> {
    if (this.#started) return
    this.#started = true
    setClientHooks(this.clientHooks())
    this.#timers.push(setInterval(() => this.tick(), 1000))
    await this.loadSession()
  }

  stop(): void {
    for (const t of this.#timers) clearInterval(t)
    this.#timers = []
    this.#shared?.close()
    this.#shared = undefined
    this.#started = false
  }

  // tick is the passing of time: the connection goes stale, and the state
  // is fetched whole when it is due.
  tick(): void {
    const c = connOf(this.#s, this.#now(), this.#mono())
    if (c.conn !== this.#s.conn || c.since !== this.#s.connSince) this.#set({})
    if (this.#s.auth === 'signed-in' && this.#s.state && this.#now() - this.#lastFull >= fullFetchEvery) void this.fetchState(false)
  }

  // Sessions

  async loadSession(): Promise<void> {
    try {
      const a = await this.#request<Session>('GET', '/api/session')
      this.#nodeClock(a)
      if (a.body) this.#signedIn(a.body)
      return
    } catch (e) {
      if (!(e instanceof ApiError) || e.status !== 401) {
        this.#set({ auth: 'signed-out', authError: e instanceof ApiError ? e : undefined })
        return
      }
      const unauth = unauthenticatedOf(e)
      this.#set({ unauthenticated: unauth })
      // After an explicit sign-out the page does not sign the ticket's user
      // in again by itself: that would undo the sign-out at once.
      if (unauth.ticket && readItem(this.#storages.session, signedOutKey) === null) {
        // a refusal is kept in authError, for the sign-in page to say
        await this.signInTicket().catch(() => undefined)
        return
      }
      this.#set({ auth: 'signed-out' })
    }
  }

  async signInTicket(): Promise<void> {
    writeItem(this.#storages.session, signedOutKey, null)
    await this.#signIn(() => this.#request<Session>('POST', '/api/session/ticket', {}))
  }

  async signInToken(token: string): Promise<void> {
    await this.#signIn(() => this.#request<Session>('POST', '/api/session/token', { token }))
  }

  async #signIn(call: () => Promise<Answer<Session>>): Promise<void> {
    try {
      const a = await call()
      this.#nodeClock(a)
      if (a.body) this.#signedIn(a.body)
    } catch (e) {
      this.#set({ auth: this.#s.signInNeeded ? this.#s.auth : 'signed-out', authError: e instanceof ApiError ? e : undefined })
      throw e
    }
  }

  // nodeClock measures, once, how far the browser's clock is from the node's.
  #nodeClock(a: Answer<unknown>): void {
    const date = a.date ? Date.parse(a.date) : NaN
    if (this.#nodeOffset === undefined && Number.isFinite(date)) this.#nodeOffset = this.#now() - date
  }

  #signedIn(session: Session): void {
    const again = this.#s.signInNeeded
    this.#ticketTried = false
    this.#lastActive = this.#now()
    this.#set({ session, auth: 'signed-in', signInNeeded: false, authError: undefined, unauthenticated: undefined })
    if (!this.#shared) this.connect()
    else if (again) {
      this.#shared.resume()
      void this.fetchState(true)
    }
  }

  async signOut(): Promise<void> {
    writeItem(this.#storages.session, signedOutKey, '1')
    try {
      await this.#request('DELETE', '/api/session')
    } catch {
      // signed out at the web process already, or it is away: the page forgets the session either way
    }
    this.#shared?.close()
    this.#shared = undefined
    this.#set({ session: undefined, auth: 'signed-out', signInNeeded: false, unauthenticated: { code: 'unauthenticated', methods: ['ticket', 'token'], ticket: true } })
  }

  signedOutHere(): boolean {
    return readItem(this.#storages.session, signedOutKey) !== null
  }

  // touch tells the web process the user is reading the page.
  async touch(): Promise<void> {
    try {
      await this.#request('POST', '/api/session/touch', {})
      this.#lastActive = this.#now()
      await this.refreshSession()
    } catch {
      // a 401 opens the dialog by itself
    }
  }

  // refreshSession reads the session again without counting as activity:
  // another tab may have kept it alive, or the role changed.
  async refreshSession(): Promise<void> {
    try {
      const a = await this.#request<Session>('GET', '/api/session', undefined, { background: true })
      this.#nodeClock(a)
      if (a.body) this.#set({ session: a.body, signInNeeded: false })
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) this.#lost(e.body)
    }
  }

  // lost is a session found gone by a call or by the stream: the dialog
  // opens over the page. It offers the Proxmox VE session of the browser
  // when there is one, and takes it at once unless the user signed out in
  // this tab: the Proxmox VE interface renews its ticket, and pco's session
  // only idled out.
  #lost(body?: unknown): void {
    if (this.#s.auth !== 'signed-in') return
    if (!isUnauthenticated(body)) {
      // The answer does not say how to sign in; /api/session does.
      this.#set({ signInNeeded: true })
      void this.#learnSignIn()
      return
    }
    this.#set({ signInNeeded: true, unauthenticated: body })
    if (body.ticket && !this.signedOutHere() && !this.#ticketTried) {
      this.#ticketTried = true
      void this.signInTicket().catch(() => undefined)
    }
  }

  async #learnSignIn(): Promise<void> {
    try {
      const a = await this.#request<Session>('GET', '/api/session', undefined, { background: true })
      if (a.body) this.#signedIn(a.body)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401 && isUnauthenticated(e.body)) this.#lost(e.body)
    }
  }

  // idleDeadline is when the session idles out, on the browser's clock: 30
  // minutes after the last activity of this tab, or later when the web
  // process says so (another tab kept it alive), its times moved onto the
  // browser's clock.
  idleDeadline(): number | undefined {
    const s = this.#s.session
    if (!s) return undefined
    const offset = this.#nodeOffset ?? 0
    const idle = Math.max(this.#lastActive + idleTimeout, Date.parse(s.idleExpiresAt) + offset)
    return Math.min(idle, Date.parse(s.expiresAt) + offset)
  }

  setDoctorLast(v: { fail: number; at: string }): void {
    writeItem(this.#storages.local, doctorKey, JSON.stringify(v))
    this.#set({ doctorLast: v })
  }

  // The user closed the sign-in dialog; the next call without a session
  // opens it again.
  dismissSignIn(): void {
    this.#set({ signInNeeded: false })
  }

  // A part of the page did not load: it was built for another version.
  markSkew(): void {
    this.#set({ skew: true })
  }

  // The stream

  connect(): void {
    this.#shared = this.#share({
      onNotice: (n, id, age) => this.notice(n, id, age),
      onLink: (l) => this.link(l),
      lastEventId: () => this.#lastId,
    })
  }

  link(l: Link): void {
    if (l.state === 'signed-out') {
      this.#set({ link: l })
      this.#lost()
      return
    }
    this.#set({ link: l })
    // The user signed in again in another tab: the stream is back, and so
    // is the session of this one.
    if (l.state === 'open' && this.#s.signInNeeded) void this.refreshSession()
  }

  // notice takes a notice of the stream; age is how long ago another tab
  // received it, for one it passes on later, and is not given for one that
  // is live.
  notice(n: Notice, id = '', age?: number): void {
    if (id) this.#lastId = id
    switch (n.kind) {
      case 'hello': {
        const h = n.data
        const known = this.#s.hello
        const loadedVersion = this.#s.loadedVersion || h.version
        const skew = this.#s.skew || h.version !== loadedVersion
        if (known && known.boot !== h.boot) {
          this.#drop({ hello: h, loadedVersion, skew })
        } else {
          this.#set({ hello: h, loadedVersion, skew })
          if (!known) this.#loadAll()
        }
        // A stream that begins again follows the end of another, and one of a
        // reader ends when the guests it sees change, which the digest of
        // the daemon does not tell: the conditional read does, with an ETag
        // for each set of guests. The hello the leader of the tabs replays
        // for a tab that opens has an age, and is no stream that began.
        const again = known !== undefined && known.boot === h.boot && age === undefined
        if (again || (h.digest && h.digest !== this.#s.state?.digest)) void this.fetchState(true)
        break
      }
      case 'state':
        this.#receivedFor = n.data.finishedAt
        this.#set({ times: n.data, receivedAt: this.#mono() - (age ?? 0) })
        if (n.data.digest !== this.#s.state?.digest) void this.fetchState(true)
        break
      case 'event':
        this.#set({ events: appendEvents(this.#s.events, [n.data]) })
        if (n.data.kind === 'admin') void this.#loadSettings()
        break
      case 'gap': {
        const g = n.data
        if (this.#s.gaps.some((x) => x.boot === g.boot && x.from === g.from && x.to === g.to)) break
        this.#set({ gaps: [...this.#s.gaps, g].slice(-maxGaps) })
        break
      }
      case 'traffic':
        this.#set({ traffic: mergeTraffic(this.#s.traffic, n.data) })
        break
      case 'reset':
        // A traffic notice was lost, one that may have told of a route that
        // stopped: the figures are read again, and nothing else is dropped.
        if (n.data.reason === resetTraffic) void this.#loadTraffic()
        else this.#drop({})
        break
      case 'upstream': {
        const wasDown = !this.#s.upstream.up
        this.#downByCall = false
        this.#set({ upstream: n.data })
        if (wasDown && n.data.up) void this.fetchState(true)
        break
      }
    }
  }

  // drop forgets what belongs to another process of the daemon, and fetches
  // it again.
  #drop(patch: Partial<AppState>): void {
    this.#etag = undefined
    this.#receivedFor = undefined
    this.#set({ ...patch, state: undefined, times: undefined, receivedAt: undefined, traffic: undefined, events: [], gaps: [] })
    this.#loadAll()
    void this.fetchState(false)
  }

  #loadAll(): void {
    void this.#loadEvents()
    void this.#loadTraffic()
    void this.#loadSettings()
  }

  async #loadEvents(): Promise<void> {
    try {
      const a = await this.#request<Event[]>('GET', `/api/v1/events?limit=${maxEvents}`, undefined, { background: true })
      this.#set({ events: appendEvents(this.#s.events, a.body ?? []) })
    } catch {
      // the stream brings the next ones
    }
  }

  async #loadTraffic(): Promise<void> {
    try {
      const a = await this.#request<TrafficView>('GET', '/api/v1/traffic', undefined, { background: true })
      if (a.body) this.#set({ traffic: a.body })
    } catch {
      // the next notice brings the figures
    }
  }

  async #loadSettings(): Promise<void> {
    try {
      const a = await this.#request<SettingsView>('GET', '/api/v1/settings', undefined, { background: true })
      if (a.body) this.#set({ settings: a.body })
    } catch {
      // shown without them
    }
  }

  // fetchState reads the state, naming the one held when conditional: the
  // web process answers 304 while the digest is the same.
  async fetchState(conditional: boolean): Promise<void> {
    if (this.#fetching) {
      this.#fetchAgain = true
      return
    }
    this.#fetching = true
    // A whole fetch that fails is not tried again on every tick.
    if (!conditional) this.#lastFull = this.#now()
    try {
      const a = await this.#request<State>('GET', '/api/v1/state', undefined, {
        background: true,
        ifNoneMatch: conditional ? this.#etag : undefined,
      })
      if (a.status !== 304 && a.body) {
        const st = a.body
        this.#etag = a.etag ?? st.digest
        this.#lastFull = this.#now()
        const patch: Partial<AppState> = { state: { ...st, routes: shareRoutes(this.#s.state?.routes, st.routes) } }
        // The data is as new as the cycle it is of: a state that shows a
        // later cycle than the page knew of (on loading, after a reconnect)
        // is news now, the same cycle fetched again is not.
        if (!zeroTime(st.finishedAt) && (this.#receivedFor === undefined || Date.parse(st.finishedAt ?? '') > Date.parse(this.#receivedFor))) {
          this.#receivedFor = st.finishedAt
          patch.receivedAt = this.#mono()
        }
        this.#set(patch)
      }
    } catch {
      // the next notice, or the next tick, tries again
    } finally {
      this.#fetching = false
    }
    if (this.#fetchAgain) {
      this.#fetchAgain = false
      const wanted = this.#s.times?.digest ?? this.#s.hello?.digest
      if (wanted && wanted !== this.#s.state?.digest) await this.fetchState(true)
    }
  }
}

function unauthenticatedOf(e: ApiError): Unauthenticated {
  return isUnauthenticated(e.body) ? e.body : { code: 'unauthenticated', methods: ['ticket', 'token'], ticket: false }
}

function browserShare(o: { onNotice: (n: Notice, id: string, age?: number) => void; onLink: (l: Link) => void; lastEventId: () => string }): Shared {
  const locks: Locks | undefined = typeof navigator !== 'undefined' && 'locks' in navigator ? navigator.locks : undefined
  const channel = typeof BroadcastChannel === 'undefined' ? undefined : new BroadcastChannel(channelName)
  return shareStream({ ...o, locks, channel, open: openStream })
}

export const appStore = new AppStore()

const StoreContext = createContext<AppStore>(appStore)

export function StoreProvider({ store, children }: { store: AppStore; children: ReactNode }) {
  return createElement(StoreContext, { value: store }, children)
}

export function useStore(): AppStore {
  return useContext(StoreContext)
}

// useApp gives what select picks of the state, and renders again when that
// changes.
export function useApp<T>(select: (s: AppState) => T): T {
  const store = useContext(StoreContext)
  const cache = useRef<{ s: AppState; select: (s: AppState) => T; v: T } | undefined>(undefined)
  const snapshot = () => {
    const s = store.get()
    const c = cache.current
    if (c && c.s === s && c.select === select) return c.v
    const v = select(s)
    cache.current = { s, select, v: c && Object.is(c.v, v) ? c.v : v }
    return v
  }
  return useSyncExternalStore(store.subscribe, snapshot)
}

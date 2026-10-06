// The one way the page calls pco web: JSON in and out, the session's CSRF
// token on every call that changes something, and a limit a little longer
// than the gateway's for that call (spec-ui 7.3, 9.3).

import { ApiError, codeNetwork, codeTimeout, type ErrorFields } from './errors'

export { ApiError } from './errors'

export type Method = 'GET' | 'POST' | 'PUT' | 'DELETE'

// What the rest of the page learns from the calls; the store sets them.
export interface ClientHooks {
  csrf: () => string | undefined
  // A call of /api/v1 found no session: the sign-in dialog opens over the
  // page, which keeps what it shows.
  unauthenticated: () => void
  // The daemon does not know the route or the method: it is another
  // version than the page.
  versionSkew: () => void
  daemonUnreachable: () => void
  // The daemon answered a call of /api/v1.
  daemonAnswered: () => void
  // A call the user made: the web process counts it as activity.
  active: () => void
}

const quiet: ClientHooks = {
  csrf: () => undefined,
  unauthenticated: () => {},
  versionSkew: () => {},
  daemonUnreachable: () => {},
  daemonAnswered: () => {},
  active: () => {},
}

let hooks: ClientHooks = quiet

export function setClientHooks(h: Partial<ClientHooks>): void {
  hooks = { ...quiet, ...h }
}

// The gateway's timeouts (P5): reads 10 s, writes 60 s, apply and the
// credential calls 75 s, diagnosis 30 s, doctor 60 s. The page waits 5 s
// longer, so that the gateway's answer comes first.
const slack = 5_000

export function timeoutFor(method: Method, path: string): number {
  const p = path.split('?')[0] ?? ''
  if (p === '/api/v1/diagnose') return 30_000 + slack
  if (p === '/api/v1/doctor') return 60_000 + slack
  if (method === 'GET') return 10_000 + slack
  if (p === '/api/v1/apply' || p === '/api/v1/credentials' || /^\/api\/v1\/credentials\/[^/]+\/check$/.test(p)) return 75_000 + slack
  // The sign-in asks Proxmox VE, 5 s a call, at most twice.
  if (p.startsWith('/api/session')) return 10_000 + slack
  return 60_000 + slack
}

export interface CallOptions {
  // A call the page makes by itself, not the user: it does not keep the
  // session from idling out (spec-ui 8.2).
  background?: boolean
  timeoutMs?: number
}

export interface RequestOptions extends CallOptions {
  ifNoneMatch?: string
}

export interface Answer<T = unknown> {
  status: number
  body: T | undefined // undefined for 204 and 304
  etag?: string
}

const isV1 = (path: string) => path.startsWith('/api/v1/')

// request makes the call and answers with its status, body and ETag; an
// answer that is not 2xx or 304 is thrown as an ApiError.
export async function request<T = unknown>(method: Method, path: string, body?: unknown, opts: RequestOptions = {}): Promise<Answer<T>> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (method !== 'GET') {
    // The web process refuses a change that is not JSON or lacks the token.
    headers['Content-Type'] = 'application/json'
    const csrf = hooks.csrf()
    if (csrf) headers['Pco-Csrf'] = csrf
  }
  if (opts.background) headers['Pco-Background'] = '1'
  if (opts.ifNoneMatch) headers['If-None-Match'] = `"${opts.ifNoneMatch}"`

  const write = method !== 'GET'
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), opts.timeoutMs ?? timeoutFor(method, path))
  let res: Response
  try {
    res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
      cache: 'no-store',
      signal: controller.signal,
    })
  } catch (err) {
    clearTimeout(timer)
    // A write is never sent again by itself: it may have reached the daemon.
    if (controller.signal.aborted) throw new ApiError(0, { code: codeTimeout, error: 'no answer in time' }, write)
    throw new ApiError(0, { code: codeNetwork, error: err instanceof Error ? err.message : String(err) }, write)
  }
  let text: string
  try {
    text = await res.text()
  } catch {
    clearTimeout(timer)
    throw new ApiError(0, { code: controller.signal.aborted ? codeTimeout : codeNetwork, error: 'the answer was cut off' }, write)
  }
  clearTimeout(timer)

  if (!opts.background) hooks.active()
  if (res.status === 304) {
    if (isV1(path)) hooks.daemonAnswered()
    return { status: 304, body: undefined, etag: etagOf(res) }
  }
  let parsed: unknown
  if (text !== '') {
    try {
      parsed = JSON.parse(text)
    } catch {
      parsed = undefined
    }
  }
  if (res.ok) {
    if (isV1(path)) hooks.daemonAnswered()
    return { status: res.status, body: parsed as T | undefined, etag: etagOf(res) }
  }
  const fields: ErrorFields = typeof parsed === 'object' && parsed !== null ? (parsed as ErrorFields) : { error: res.statusText }
  const e = new ApiError(res.status, { ...fields, code: fields.code ?? (res.status === 401 ? 'unauthenticated' : 'internal') })
  if (isV1(path)) {
    switch (e.code) {
      case 'unauthenticated':
        hooks.unauthenticated()
        break
      case 'no_route':
      case 'method_not_allowed':
        hooks.versionSkew()
        break
      case 'daemon_unreachable':
        hooks.daemonUnreachable()
        break
      case 'unsupported_media_type':
        console.error(`pco web refused the request ${method} ${path} as not JSON: ${e.message}`)
        break
    }
  }
  throw e
}

function etagOf(res: Response): string | undefined {
  const tag = res.headers.get('ETag')
  return tag ? tag.replace(/^W\//, '').replace(/^"|"$/g, '') : undefined
}

// api makes a call and answers with its body.
export async function api<T>(method: Method, path: string, body?: unknown, opts?: CallOptions): Promise<T> {
  const answer = await request<T>(method, path, body, opts)
  return answer.body as T
}

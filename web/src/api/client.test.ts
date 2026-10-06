import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api, ApiError, request, setClientHooks, timeoutFor } from './client'
import { explain, timeoutWriteText } from './errors'

type Call = { url: string; init: RequestInit }

let calls: Call[]

function answer(status: number, body?: unknown, headers: Record<string, string> = {}) {
  return vi.fn(async (url: string, init: RequestInit) => {
    calls.push({ url, init })
    return new Response(body === undefined ? null : JSON.stringify(body), { status, headers })
  })
}

function headersOf(i = 0): Record<string, string> {
  return (calls[i]?.init.headers ?? {}) as Record<string, string>
}

beforeEach(() => {
  calls = []
  setClientHooks({ csrf: () => 'token-1' })
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
  setClientHooks({})
})

describe('the headers', () => {
  test('Pco-Csrf and JSON on a POST, not on a GET', async () => {
    vi.stubGlobal('fetch', answer(200, { ok: true }))
    await api('POST', '/api/v1/sync', {})
    await api('GET', '/api/v1/state')
    expect(headersOf(0)['Pco-Csrf']).toBe('token-1')
    expect(headersOf(0)['Content-Type']).toBe('application/json')
    expect(calls[0]?.init.body).toBe('{}')
    expect(calls[0]?.init.credentials).toBe('same-origin')
    expect(headersOf(1)['Pco-Csrf']).toBeUndefined()
    expect(headersOf(1)['Content-Type']).toBeUndefined()
    expect(calls[1]?.init.body).toBeUndefined()
  })

  test('Pco-Background only when asked', async () => {
    const active = vi.fn()
    setClientHooks({ active })
    vi.stubGlobal('fetch', answer(200, []))
    await api('GET', '/api/v1/events', undefined, { background: true })
    await api('GET', '/api/v1/events')
    expect(headersOf(0)['Pco-Background']).toBe('1')
    expect(headersOf(1)['Pco-Background']).toBeUndefined()
    expect(active).toHaveBeenCalledTimes(1)
  })

  test('If-None-Match, and a 304 keeps the body out', async () => {
    vi.stubGlobal('fetch', answer(304, undefined, { ETag: '"5e0c1f7a92b4d3e8"' }))
    const got = await request('GET', '/api/v1/state', undefined, { background: true, ifNoneMatch: '5e0c1f7a92b4d3e8' })
    expect(headersOf()['If-None-Match']).toBe('"5e0c1f7a92b4d3e8"')
    expect(got).toEqual({ status: 304, body: undefined, etag: '5e0c1f7a92b4d3e8' })
  })
})

describe("the timeouts are the gateway's and 5 s", () => {
  test.each([
    ['GET', '/api/v1/state', 15_000],
    ['GET', '/api/v1/events?after=3', 15_000],
    ['POST', '/api/v1/sync', 65_000],
    ['PUT', '/api/v1/settings', 65_000],
    ['DELETE', '/api/v1/routes/manual/status?rev=2', 65_000],
    ['POST', '/api/v1/apply', 80_000],
    ['POST', '/api/v1/credentials', 80_000],
    ['POST', '/api/v1/credentials/cred1/check', 80_000],
    ['POST', '/api/v1/diagnose', 35_000],
    ['POST', '/api/v1/doctor', 65_000],
  ] as const)('%s %s', (method, path, ms) => {
    expect(timeoutFor(method, path)).toBe(ms)
  })

  test('a write without an answer says the outcome is unknown, and is sent once', async () => {
    vi.useFakeTimers()
    const fetch = vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_, reject) => {
          init.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
        }),
    )
    vi.stubGlobal('fetch', fetch)
    const call = api('POST', '/api/v1/apply', { confirmDeletes: false, offer: '' }).catch((e: unknown) => e)
    await vi.advanceTimersByTimeAsync(79_999)
    expect(fetch).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    const e = await call
    expect(e).toBeInstanceOf(ApiError)
    expect(explain(e as ApiError).text).toBe(timeoutWriteText)
    expect(explain(e as ApiError).remedy).toBe('reload')
    await vi.advanceTimersByTimeAsync(120_000)
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  test('a read without an answer is not called a write', async () => {
    vi.useFakeTimers()
    vi.stubGlobal(
      'fetch',
      vi.fn(
        (_url: string, init: RequestInit) =>
          new Promise<Response>((_, reject) => init.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))),
      ),
    )
    const call = api('GET', '/api/v1/state').catch((e: unknown) => e)
    await vi.advanceTimersByTimeAsync(15_000)
    const e = (await call) as ApiError
    expect(e.code).toBe('timeout')
    expect(explain(e).text).not.toContain('outcome')
  })
})

describe('the error codes of the daemon and of the web process', () => {
  const at = new Date('2026-10-05T10:01:05Z')
  test.each([
    [400, { error: 'gateTag: not a tag', code: 'invalid', field: 'gateTag' }, 'gateTag: not a tag', undefined],
    [404, { error: 'no such route', code: 'not_found' }, 'This no longer exists; the view shows what there is now.', 'refresh'],
    [409, { error: 'the settings changed since revision 7', code: 'refused' }, 'the settings changed since revision 7', 'look-again'],
    [503, { error: 'a cycle is running; try again', code: 'unavailable' }, 'a cycle is running; try again', 'try-again'],
    [403, { error: 'your role cannot do this: it needs Sys.Modify on /', code: 'forbidden', missing: 'Sys.Modify' }, 'your role cannot do this: it needs Sys.Modify on /', undefined],
    [403, { error: 'not allowed', code: 'forbidden' }, "pco's web process may not use the daemon's socket; the troubleshooting guide says why.", undefined],
    [401, { code: 'unauthenticated', methods: ['ticket', 'token'], ticket: true }, 'Your session has ended. Sign in again; what you did last was not sent again.', 'sign-in'],
    [401, { error: 'Proxmox VE did not accept the session of this browser; sign in to Proxmox VE again', code: 'ticket_invalid' }, 'Proxmox VE did not accept the session of this browser; sign in to Proxmox VE again', undefined],
    [503, { error: 'x', code: 'proxmox_unreachable' }, "Proxmox VE on this node does not answer, or presents another certificate than the node's.", 'try-again'],
    [502, { error: 'x', code: 'daemon_unreachable' }, 'The pco daemon does not answer. Published routes keep working; nothing changes until it is back.', 'try-again'],
    [429, { error: 'x', code: 'rate_limited', retryAfter: 30 }, 'Try again in 30 s.', undefined],
    [413, { error: 'x', code: 'too_large' }, 'The request is too large.', undefined],
    [404, { error: 'no such route', code: 'no_route' }, 'The daemon is another version than this page. Reload the page to use the new one.', 'reload'],
    [405, { error: 'method not allowed', code: 'method_not_allowed' }, 'The daemon is another version than this page. Reload the page to use the new one.', 'reload'],
  ])('%i %j', async (status, body, text, remedy) => {
    vi.stubGlobal('fetch', answer(status, body))
    const e = await api('POST', '/api/v1/sync', {}).catch((err: unknown) => err)
    expect(e).toBeInstanceOf(ApiError)
    const ae = e as ApiError
    expect(ae.status).toBe(status)
    expect(ae.code).toBe(body.code)
    expect(explain(ae, at)).toMatchObject({ text, remedy })
  })

  test('the fields of the body are kept', async () => {
    vi.stubGlobal('fetch', answer(429, { error: 'too many', code: 'rate_limited', retryAfter: 12, field: 'token', missing: 'Sys.Audit' }))
    const e = (await api('GET', '/api/v1/state').catch((err: unknown) => err)) as ApiError
    expect([e.retryAfter, e.field, e.missing]).toEqual([12, 'token', 'Sys.Audit'])
  })

  test('internal and what the page does not know: the time and the journal', async () => {
    vi.stubGlobal('fetch', answer(500, { error: 'boom', code: 'internal' }))
    const e = (await api('GET', '/api/v1/state').catch((err: unknown) => err)) as ApiError
    expect(explain(e).text).toMatch(/^Something went wrong in pco at \d\d:\d\d:\d\d\. journalctl -u pco on the node has the details\.$/)
  })

  test('a 401 of the API opens the sign-in; no_route and method_not_allowed raise the reload banner', async () => {
    const unauthenticated = vi.fn()
    const versionSkew = vi.fn()
    const daemonUnreachable = vi.fn()
    setClientHooks({ unauthenticated, versionSkew, daemonUnreachable })
    for (const [status, code] of [
      [401, 'unauthenticated'],
      [404, 'no_route'],
      [405, 'method_not_allowed'],
      [502, 'daemon_unreachable'],
      [404, 'not_found'],
    ] as const) {
      vi.stubGlobal('fetch', answer(status, { error: '', code }))
      await api('GET', '/api/v1/state').catch(() => undefined)
    }
    expect(unauthenticated).toHaveBeenCalledTimes(1)
    expect(versionSkew).toHaveBeenCalledTimes(2)
    expect(daemonUnreachable).toHaveBeenCalledTimes(1)
  })

  test("a 401 of the session routes is the caller's to handle", async () => {
    const unauthenticated = vi.fn()
    setClientHooks({ unauthenticated })
    vi.stubGlobal('fetch', answer(401, { code: 'unauthenticated', methods: ['ticket'], ticket: false }))
    await expect(api('GET', '/api/session')).rejects.toBeInstanceOf(ApiError)
    expect(unauthenticated).not.toHaveBeenCalled()
  })

  test('the web process cannot be reached', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('Failed to fetch')
      }),
    )
    const e = (await api('GET', '/api/v1/state').catch((err: unknown) => err)) as ApiError
    expect(e.code).toBe('network')
    expect(e.status).toBe(0)
  })
})

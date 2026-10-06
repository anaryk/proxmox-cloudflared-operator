import type { APIRequestContext, APIResponse } from '@playwright/test'

import { admin, contentSecurityPolicy, expect, test } from './fixtures'

// Requests as another site or a script makes them, without a page: each
// check of a call that changes something refuses it before the daemon hears
// of it, and every answer carries pco web's headers and none of CORS. The
// browser plays no part here, so playwright.config.ts runs this file in one
// project only.

const foreign = 'https://pco.example.net'

// The calls the page makes that change something, and the two reads that
// are POSTs for the same checks.
const calls = [
  { path: '/api/v1/sync', body: {}, method: 'Trigger' },
  { path: '/api/v1/diagnose', body: { hostname: 'app.example.com' }, method: 'Diagnose' },
  { path: '/api/v1/doctor', body: {}, method: 'Doctor' },
]

const notOwnPage = "the request does not come from pco's own page"

// Each way a request differs from the page's own, and how pco web refuses it.
const refusals: { why: string; headers: Record<string, string>; status: number; error: string }[] = [
  { why: 'without the token of the session', headers: { 'Pco-Csrf': '' }, status: 403, error: 'the request does not carry the token of this session' },
  { why: 'that is not JSON', headers: { 'Content-Type': 'text/plain' }, status: 415, error: 'the request is not JSON' },
  { why: 'from another origin', headers: { Origin: foreign }, status: 403, error: notOwnPage },
  { why: 'from a page of the same site', headers: { 'Sec-Fetch-Site': 'same-site' }, status: 403, error: notOwnPage },
  {
    why: 'to a name pco does not answer to',
    headers: { Host: 'pco.example.net', Origin: foreign },
    status: 403,
    error: 'pco does not answer to the name in the address bar; open it under the node',
  },
]

async function signIn(request: APIRequestContext, origin: string): Promise<string> {
  const res = await request.post('/api/session/token', { data: { token: admin.token }, headers: { Origin: origin } })
  expect(res.status(), await res.text()).toBe(200)
  return ((await res.json()) as { csrf: string }).csrf
}

// send posts body as the page does, but for the headers given, of which an
// empty one is left out.
function send(request: APIRequestContext, path: string, body: unknown, headers: Record<string, string>): Promise<APIResponse> {
  const sent = Object.fromEntries(Object.entries(headers).filter(([, v]) => v !== ''))
  return request.post(path, { data: JSON.stringify(body), headers: sent })
}

test('a call that changes something is refused unless it comes from the page', async ({ request, pco, fake }) => {
  const csrf = await signIn(request, pco.url)
  const page = { Origin: pco.url, 'Content-Type': 'application/json', 'Pco-Csrf': csrf }
  await fake.clearCalls()

  for (const call of calls) {
    for (const r of refusals) {
      const res = await send(request, call.path, call.body, { ...page, ...r.headers })
      expect(res.status(), `${call.path} ${r.why}`).toBe(r.status)
      expect(((await res.json()) as { error: string }).error, `${call.path} ${r.why}`).toContain(r.error)
    }
  }
  expect(await fake.calls(), 'what reached the daemon').toEqual([])

  for (const call of calls) {
    const res = await send(request, call.path, call.body, page)
    expect(res.ok(), `${call.path} from the page: ${await res.text()}`).toBe(true)
  }
  expect((await fake.calls()).map((c) => c.method)).toEqual(calls.map((c) => c.method))
})

// answers asks pco web for a page, an asset, the licences, a path it does
// not have, the session and the state, all as another site would, and the
// preflight of a call.
async function answers(request: APIRequestContext): Promise<APIResponse[]> {
  const index = await request.get('/', { headers: { Origin: foreign } })
  const asset = /\/assets\/[\w.-]+\.js/.exec(await index.text())?.[0]
  expect(asset, 'the script of index.html').toBeDefined()
  const preflight = await request.fetch('/api/v1/sync', {
    method: 'OPTIONS',
    headers: { Origin: foreign, 'Access-Control-Request-Method': 'POST', 'Access-Control-Request-Headers': 'content-type, pco-csrf' },
  })
  expect(preflight.status(), 'OPTIONS').toBe(405)
  return [
    index,
    preflight,
    ...(await Promise.all(
      ['/routes', asset ?? '', '/licenses.txt', '/nothing-here', '/api/session', '/api/v1/state', '/api/v1/nothing-here'].map((path) =>
        request.get(path, { headers: { Origin: foreign } }),
      ),
    )),
    await request.post('/api/v1/sync', { data: {}, headers: { Origin: foreign } }),
  ]
}

test('every answer has the policy, no header of CORS and no HSTS', async ({ request, pco }) => {
  await signIn(request, pco.url)
  for (const res of await answers(request)) {
    const h = res.headers()
    expect(h['content-security-policy'], res.url()).toBe(contentSecurityPolicy)
    expect(
      Object.keys(h).filter((k) => k.startsWith('access-control-')),
      res.url(),
    ).toEqual([])
    expect(h['strict-transport-security'], res.url()).toBeUndefined()
  }
})

test.describe('with PCO_WEB_HSTS=1', () => {
  test.use({ hsts: true })

  test('every answer has HSTS', async ({ request, pco }) => {
    await signIn(request, pco.url)
    for (const res of await answers(request)) {
      expect(res.headers()['strict-transport-security'], res.url()).toBe('max-age=31536000')
    }
  })
})

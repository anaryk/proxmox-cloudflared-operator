// @vitest-environment node
// The mock runs in the development server, in Node.

import { expect, test } from 'vitest'

import populated from '../fixtures/populated.json'
import { Mock } from './mock'

test('the state is the fixture, named by its digest', () => {
  const m = new Mock()
  const a = m.answer('GET', '/api/v1/state', {})
  expect(a.status).toBe(200)
  expect(a.body).toEqual(populated)
  expect(m.answer('GET', '/api/v1/state', { 'if-none-match': `"${populated.digest}"` }).status).toBe(304)
})

test('another fixture, and the large one', () => {
  expect((new Mock('untagged').answer('GET', '/api/v1/state', {}).body as { gateTagged: number }).gateTagged).toBe(0)
  expect((new Mock('large').answer('GET', '/api/v1/state', {}).body as { routes: unknown[] }).routes).toHaveLength(1000)
  const scenario = new Mock('scenario-populated').answer('GET', '/api/v1/state', {}).body as { routes: { state: string }[] }
  expect(scenario.routes.some((r) => r.state === 'rejected')).toBe(true)
})

test('a sign-out holds until the page signs in again', () => {
  const m = new Mock()
  expect(m.answer('GET', '/api/session', {}).status).toBe(200)
  expect(m.answer('DELETE', '/api/session', {}).status).toBe(204)
  expect(m.answer('GET', '/api/session', {}).status).toBe(401)
  expect(m.answer('GET', '/api/v1/state', {}).status).toBe(401)
  expect(m.answer('POST', '/api/session/ticket', {}).status).toBe(200)
  expect(m.answer('GET', '/api/v1/state', {}).status).toBe(200)
})

test('the doctor answers with its findings, though it is a POST', () => {
  const a = new Mock().answer('POST', '/api/v1/doctor', {})
  expect(a.status).toBe(200)
  expect((a.body as { check: string; level: string }[]).map((f) => f.level)).toContain('fail')
})

test('writes change nothing; an unknown read is not found', () => {
  const m = new Mock()
  expect(m.answer('POST', '/api/v1/apply', {})).toMatchObject({ status: 503, body: { code: 'unavailable' } })
  expect(m.answer('GET', '/api/v1/nothing', {})).toMatchObject({ status: 404, body: { code: 'not_found' } })
})

test('a diagnosis gets the one of the fixture, and a route its series', () => {
  const m = new Mock('scenario-populated')
  const d = m.answer('POST', '/api/v1/diagnose', {})
  expect(d.status).toBe(200)
  expect((d.body as { name: string }[]).map((s) => s.name)).toEqual(['route', 'zone', 'dns', 'ingress', 'connector', 'identity', 'tcp', 'http'])
  const r = m.answer('GET', '/api/v1/traffic/route?hostname=store.example.com', {})
  const series = r.body as { target: string; shared: number; samples: { at: string }[] }
  expect([series.target, series.shared, series.samples.length, series.samples.at(-1)?.at]).toEqual(['10.0.0.40:80', 1, 180, '2026-10-01T12:00:05Z'])
  expect(m.answer('GET', '/api/v1/traffic/route?hostname=old.example.com', {}).status).toBe(404)
  // a write still changes nothing
  expect(m.answer('POST', '/api/v1/sync', {}).status).toBe(503)
})

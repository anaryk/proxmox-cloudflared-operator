import { describe, expect, test } from 'vitest'

import type { CredentialView, State } from '../../api/types.gen'
import firstRun from '../../fixtures/first-run.json'
import populated from '../../fixtures/populated.json'
import { needsInstall, progressOf, type StepId } from './steps'

const fresh = firstRun as unknown as State
const full = populated as unknown as State
const good = full.credentials[0] as CredentialView
const at = '2026-10-01T12:00:00Z'

const marks = (st: State) => Object.fromEntries(progressOf(st).steps.map((s) => [s.id, s.mark])) as Partial<Record<StepId, string>>

// a daemon that has run, with one credential and nothing else yet
const withToken = (c: CredentialView, patch: Partial<State> = {}): State => ({
  ...fresh,
  at,
  finishedAt: at,
  problems: [],
  credentials: [c],
  ...patch,
})

describe('without a credential', () => {
  test('the token is the step to take, nothing after it can be done, and publishing is not open', () => {
    const p = progressOf(fresh)
    expect(p.token).toBe('none')
    expect(p.current).toBe('token')
    expect(p.missing).toEqual(['token', 'route'])
    expect(marks(fresh)).toEqual({ token: 'todo', zones: 'blocked', reach: 'optional', route: 'todo', publish: 'blocked' })
  })

  test('there is no install step while the daemon runs', () => {
    expect(progressOf(fresh).installNeeded).toBe(false)
    expect(progressOf(fresh).steps.map((s) => s.id)).not.toContain('install')
  })
})

describe('the install check', () => {
  test.each([
    ['the problem asks for pco setup', { problems: ['pco is not set up on this node; run pco setup'] }],
    ['no writer identity', { problems: ['no writer identity; run pco setup'] }],
    ['the writer is unknown', { writerVerdict: 'unknown' }],
  ])('is the first step when %s', (_, patch) => {
    const st = { ...fresh, ...patch }
    expect(needsInstall(st)).toBe(true)
    const p = progressOf(st)
    expect(p.steps[0]).toEqual({ id: 'install', title: 'Install check', mark: 'problem' })
    expect(p.current).toBe('install')
  })

  test('is not needed for another problem or another verdict', () => {
    expect(needsInstall({ problems: ['the inventory is incomplete'], writerVerdict: 'stale' })).toBe(false)
    expect(needsInstall({ problems: [], writerVerdict: 'ok' })).toBe(false)
  })
})

describe('a token that cannot be used', () => {
  const bad: CredentialView = { ...good, report: good.report && { ...good.report, usable: false, deep: false } }

  test('is a problem, keeps the step open and publishing closed', () => {
    const st = withToken(bad, { routes: full.routes })
    const p = progressOf(st)
    expect(p.token).toBe('problem')
    expect(p.writeUntried).toBe(false)
    expect(p.missing).toEqual(['token'])
    expect(p.current).toBe('token')
    expect(marks(st).token).toBe('problem')
    expect(marks(st).publish).toBe('blocked')
  })

  test('that Cloudflare did not answer for is unknown, not a missing permission', () => {
    const unanswered: CredentialView = {
      ...bad,
      report: bad.report && { ...bad.report, checks: [{ capability: 'dns.read', scope: 'example.com', scopeId: 'zone1', ok: false, unanswered: true }] },
    }
    expect(progressOf(withToken(unanswered)).token).toBe('unknown')
  })

  test('that was never checked is unknown', () => {
    expect(progressOf(withToken({ id: 'c1', label: 'x', kind: 'scoped', checked: false })).token).toBe('unknown')
  })
})

describe('a token that can be used', () => {
  const shallow: CredentialView = { ...good, report: good.report && { ...good.report, deep: false } }

  test('is done; the zones wait for the first cycle that saw it', () => {
    const waiting = withToken({ ...shallow, report: shallow.report && { ...shallow.report, checkedAt: '2026-10-01T12:00:05Z' } })
    const p = progressOf(waiting)
    expect(p.token).toBe('usable')
    expect(p.zones).toBe('waiting')
    expect(p.current).toBe('route')
    expect(marks(waiting)).toMatchObject({ token: 'done', zones: 'wait', route: 'todo', publish: 'blocked' })
  })

  test('with a cycle after the check and no zone, the zones step says so', () => {
    const empty = withToken({ ...shallow, report: shallow.report && { ...shallow.report, checkedAt: '2026-10-01T11:59:00Z' } })
    expect(progressOf(empty).zones).toBe('empty')
    expect(marks(empty).zones).toBe('problem')
    expect(progressOf(empty).current).toBe('zones')
  })

  test('is a write access that was not tried until a deep check ran', () => {
    expect(progressOf(withToken(shallow)).writeUntried).toBe(true)
    expect(progressOf(withToken(good)).writeUntried).toBe(false)
  })

  test('with zones in the state they are done', () => {
    const st = withToken(good, { zones: full.zones })
    expect(progressOf(st).zones).toBe('listed')
    expect(marks(st).zones).toBe('done')
  })
})

describe('publishing', () => {
  const ready = withToken(good, { zones: full.zones, routes: full.routes, mode: 'observe' })

  test('opens with a usable credential and a route, and then it is the step to take', () => {
    const p = progressOf(ready)
    expect(p.missing).toEqual([])
    expect(p.current).toBe('publish')
    expect(marks(ready)).toMatchObject({ token: 'done', zones: 'done', route: 'done', publish: 'todo' })
  })

  test('stays closed for a route alone, saying that the token is missing', () => {
    const p = progressOf({ ...fresh, routes: full.routes })
    expect(p.missing).toEqual(['token'])
    expect(p.steps.find((s) => s.id === 'publish')?.mark).toBe('blocked')
  })

  test('stays closed for a token alone, saying that the route is missing', () => {
    expect(progressOf(withToken(good, { zones: full.zones })).missing).toEqual(['route'])
  })

  test('is done once the daemon left observe-only mode', () => {
    const p = progressOf({ ...ready, mode: 'enforce' })
    expect(p.publishing).toBe(true)
    expect(p.steps.find((s) => s.id === 'publish')?.mark).toBe('done')
  })

  test('is not done without a usable token, whatever the mode says', () => {
    const st = { ...fresh, at, mode: 'enforce' }
    expect(progressOf(st).steps.find((s) => s.id === 'publish')?.mark).toBe('blocked')
  })

  test('before the first cycle no mode says publishing', () => {
    expect(progressOf({ ...fresh, mode: 'enforce' }).publishing).toBe(false)
  })
})

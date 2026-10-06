import { describe, expect, test } from 'vitest'

import { ApiError } from '../../api/client'
import type { ManualRouteView, Settings } from '../../api/types.gen'
import manualFixture from '../../fixtures/manual-routes.json'
import settingsFixture from '../../fixtures/settings.json'
import { planRoutes } from './diff'
import { type Calls, importSteps, runSteps } from './importRun'

const stored = settingsFixture.settings as unknown as Settings
const status = (manualFixture as unknown as ManualRouteView[])[0] as ManualRouteView
const route = (id: string, over: Partial<ManualRouteView> = {}): ManualRouteView => ({ ...status, id, hostname: `${id}.example.com`, rev: 1, ...over })

function recorder(fail: Record<string, ApiError | Error> = {}) {
  const log: string[] = []
  const send = (what: string) => {
    log.push(what)
    const err = fail[what]
    return err ? Promise.reject(err) : Promise.resolve({})
  }
  const calls: Calls = {
    putSettings: (rev) => send(`PUT settings rev ${rev}`),
    createRoute: (r) => send(`POST ${r.id}`),
    updateRoute: (r, rev) => send(`PUT ${r.id} rev ${rev}`),
    deleteRoute: (id, rev) => send(`DELETE ${id} rev ${rev}`),
  }
  return { log, calls }
}

const current = [route('keep', { rev: 4 }), route('change', { rev: 5 }), route('gone', { rev: 6 })]
const incoming = [route('keep', { rev: 99 }), route('change', { rev: 99, target: { ...status.target, port: 9999 } }), route('fresh', { rev: 0 })]
const plan = planRoutes(current, incoming, 'replace')
const newSettings = { ...stored, pollInterval: '30s' }

describe('the calls of an import', () => {
  test('the settings are saved first, then the routes: added, changed, deleted, each at the revision stored', async () => {
    const { log, calls } = recorder()
    const steps = importSteps({ rev: 7, settings: newSettings, plan }, calls)
    const out = await runSteps(steps)
    expect(log).toEqual(['PUT settings rev 7', 'POST fresh', 'PUT change rev 5', 'DELETE gone rev 6'])
    expect(out).toEqual({ saved: ['settings', 'add route fresh', 'change route change', 'delete route gone'], notSaved: [] })
  })

  test('settings that did not change are not saved again', async () => {
    const { log, calls } = recorder()
    await runSteps(importSteps({ rev: 7, plan }, calls))
    expect(log).toEqual(['POST fresh', 'PUT change rev 5', 'DELETE gone rev 6'])
  })

  test('a merge deletes nothing', async () => {
    const { log, calls } = recorder()
    await runSteps(importSteps({ rev: 7, settings: newSettings, plan: planRoutes(current, incoming, 'merge') }, calls))
    expect(log).toEqual(['PUT settings rev 7', 'POST fresh', 'PUT change rev 5'])
  })

  test('with nothing to change there is no step', () => {
    expect(importSteps({ rev: 7, plan: planRoutes(current, [], 'merge') }, recorder().calls)).toEqual([])
  })
})

describe('the first refusal stops the import and the report says what was saved and what was not', () => {
  test('a route that is refused: the settings and the routes before it are saved, those after are not tried', async () => {
    const refused = new ApiError(409, { code: 'refused', error: 'refused: the manual route manual/change changed since it was read at revision 5; read it again' })
    const { log, calls } = recorder({ 'PUT change rev 5': refused })
    const out = await runSteps(importSteps({ rev: 7, settings: newSettings, plan }, calls))
    expect(log).toEqual(['PUT settings rev 7', 'POST fresh', 'PUT change rev 5'])
    expect(out.saved).toEqual(['settings', 'add route fresh'])
    expect(out.failed).toEqual({ label: 'change route change', text: refused.message, quoted: true, unknown: false })
    expect(out.notSaved).toEqual(['delete route gone'])
  })

  test('the settings refused: nothing was saved and no route is tried', async () => {
    const invalid = new ApiError(400, { code: 'invalid', error: 'pollInterval 1s: at least 5s', field: 'pollInterval' })
    const { log, calls } = recorder({ 'PUT settings rev 7': invalid })
    const out = await runSteps(importSteps({ rev: 7, settings: newSettings, plan }, calls))
    expect(log).toEqual(['PUT settings rev 7'])
    expect(out.saved).toEqual([])
    expect(out.failed?.label).toBe('settings')
    expect(out.notSaved).toEqual(['add route fresh', 'change route change', 'delete route gone'])
  })

  test('a write without an answer may have happened: the report says it is not known', async () => {
    const timeout = new ApiError(0, { code: 'timeout', error: 'no answer in time' }, true)
    const { calls } = recorder({ 'POST fresh': timeout })
    const out = await runSteps(importSteps({ rev: 7, plan }, calls))
    expect(out.failed).toMatchObject({ label: 'add route fresh', unknown: true, quoted: false })
    expect(out.failed?.text).toContain('the outcome is unknown')
  })

  test('an error that is no answer of the daemon is shown as it is', async () => {
    const { calls } = recorder({ 'POST fresh': new TypeError('boom') })
    const out = await runSteps(importSteps({ rev: 7, plan }, calls))
    expect(out.failed).toMatchObject({ label: 'add route fresh', text: 'boom', unknown: false })
  })
})

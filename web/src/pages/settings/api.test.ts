import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { setClientHooks } from '../../api/client'
import type { ManualRouteView, Settings } from '../../api/types.gen'
import manualFixture from '../../fixtures/manual-routes.json'
import settingsFixture from '../../fixtures/settings.json'
import { createRoute, deleteRoute, getRoutes, getSettings, putSettings, restartDaemon, updateRoute } from './api'

const status = (manualFixture as unknown as ManualRouteView[])[0] as ManualRouteView
const stored = settingsFixture.settings as unknown as Settings

let calls: { url: string; init: RequestInit }[]

beforeEach(() => {
  calls = []
  setClientHooks({ csrf: () => 'token-1' })
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init })
      return new Response('{}', { status: 200 })
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
  setClientHooks({})
})

const sent = (i = 0) => ({ url: calls[i]?.url, method: calls[i]?.init.method, body: calls[i]?.init.body ? JSON.parse(calls[i]?.init.body as string) : undefined })

test('the settings are read, and written at the revision they were read at', async () => {
  await getSettings()
  await putSettings(7, stored)
  expect(sent(0)).toEqual({ url: '/api/v1/settings', method: 'GET', body: undefined })
  expect(sent(1)).toEqual({ url: '/api/v1/settings', method: 'PUT', body: { rev: 7, settings: stored } })
})

test('a new route is sent without a revision', async () => {
  await getRoutes()
  await createRoute({ ...status, rev: 12 })
  expect(sent(0).url).toBe('/api/v1/routes/manual')
  expect(sent(1)).toEqual({
    url: '/api/v1/routes/manual',
    method: 'POST',
    body: { id: 'status', hostname: 'status.example.com', target: status.target, options: status.options },
  })
})

test('a route is changed at the revision given, not the one it carries', async () => {
  await updateRoute({ ...status, rev: 99 }, 2)
  expect(sent()).toEqual({
    url: '/api/v1/routes/manual/status',
    method: 'PUT',
    body: { id: 'status', rev: 2, hostname: 'status.example.com', target: status.target, options: status.options },
  })
})

test('a route is deleted at its revision, in the query', async () => {
  await deleteRoute('status', 2)
  expect(sent()).toEqual({ url: '/api/v1/routes/manual/status?rev=2', method: 'DELETE', body: undefined })
})

test('the restart is a POST with an empty body', async () => {
  await restartDaemon()
  expect(sent()).toEqual({ url: '/api/v1/daemon/restart', method: 'POST', body: {} })
})

import { cleanup, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import type { AgentView, State } from '../api/types.gen'
import cluster from '../fixtures/cluster.json'
import populated from '../fixtures/populated.json'
import { fakeStore } from '../test/store'
import { LocalAgent } from './LocalAgent'
import { renderPage, stubApi } from './testing'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const agent: AgentView = {
  node: 'pve2',
  kind: 'host',
  controller: 'pve1 (10.10.0.1)',
  silentFor: 2e9,
  epoch: { installId: 'abc123', generation: 7, nonce: 'k3x9' },
  seq: 42,
  seqAge: 3e9,
  connectors: 2,
  egressTargets: 9,
  certFingerprint: 'deadbeef'.repeat(8),
  certExpires: '2026-12-20T12:00:00Z',
  readyReplicas: { '00000000-0000-4000-8000-000000000001': 2 },
  heard: true,
  heartbeatAge: 1e9,
  problems: ['a problem of the agent'],
}

test('the agent of this node, as it sees itself', async () => {
  stubApi({ 'GET /api/v1/local/agent': { status: 200, body: agent } })
  const { store } = await fakeStore({ state: cluster })
  renderPage(store, <LocalAgent />)
  const card = await screen.findByRole('region', { name: 'Agent' })
  await screen.findByText('pve1 (10.10.0.1)')
  const text = card.textContent ?? ''
  expect(text).toContain('heard')
  expect(text).toContain('last heartbeat 1 s ago')
  expect(text).toContain('7.k3x9')
  expect(text).toContain('abc123')
  expect(text).toContain('sequence 42, applied 3 s ago')
  expect(text).toContain('2 connectors; 9 targets in its egress set')
  expect(text).toContain('00000000: 2 as the last heartbeat counted them')
  expect(text).toContain('deadbeef'.repeat(8))
  expect(text).toContain('a problem of the agent')
  expect(screen.queryByText(/does not hold the lease/)).toBeNull()
})

test('a silent controller, and a node that stood down', async () => {
  stubApi({ 'GET /api/v1/local/agent': { status: 200, body: { ...agent, silentFor: 90e9, heartbeatAge: 90e9, stoodDown: 'the controller named this node removed' } } })
  const { store } = await fakeStore({ state: cluster })
  renderPage(store, <LocalAgent />)
  const card = await screen.findByRole('region', { name: 'Agent' })
  await screen.findByText('stood down')
  expect(card.textContent).toContain('the controller named this node removed')
})

test('an agent no heartbeat came to yet says so, and no age of one', async () => {
  stubApi({ 'GET /api/v1/local/agent': { status: 200, body: { ...agent, heard: false, heartbeatAge: 0, readyReplicas: undefined } } })
  const { store } = await fakeStore({ state: cluster })
  renderPage(store, <LocalAgent />)
  const card = await screen.findByRole('region', { name: 'Agent' })
  await screen.findByText('not heard')
  expect(card.textContent).toContain('no heartbeat came yet')
  expect(card.textContent).not.toContain('last heartbeat')
})

test('on a node that does not hold the lease the page says so, and the daemon’s refusal is shown', async () => {
  stubApi({ 'GET /api/v1/local/agent': { status: 403, body: { code: 'forbidden', error: 'needs Sys.Modify on /' } } })
  const other: State = { ...(populated as unknown as State), lease: { holder: 'pve2', generation: 7, refreshAge: 0, valid: false } }
  const { store } = await fakeStore({ state: other })
  renderPage(store, <LocalAgent />)
  const note = screen.getByText((_, el) => el?.tagName === 'P' && /does not hold the lease/.test(el.textContent ?? ''))
  expect(note.textContent).toContain('pve2 is the controller')
  await screen.findByRole('alert')
})

test('a reader is told the page is for admins, the agent is not asked, and the lease is still said', async () => {
  const sent = stubApi({ 'GET /api/v1/local/agent': { status: 200, body: agent } })
  const other: State = { ...(populated as unknown as State), lease: { holder: 'pve2', generation: 7, refreshAge: 0, valid: false } }
  const { store } = await fakeStore({ state: other, session: { role: 'reader' } })
  renderPage(store, <LocalAgent />)
  const card = screen.getByRole('region', { name: 'Agent' })
  expect(card.textContent).toContain('This page is for admins')
  const note = screen.getByText((_, el) => el?.tagName === 'P' && /does not hold the lease/.test(el.textContent ?? ''))
  expect(note.textContent).toContain('pve2 is the controller')
  await new Promise((r) => setTimeout(r, 0))
  expect(sent.filter((s) => s.path.startsWith('/api/v1/local/agent'))).toEqual([])
  expect(screen.queryByRole('alert')).toBeNull()
})

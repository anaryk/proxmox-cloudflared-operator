import { fireEvent, render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import type { State } from '../api/types.gen'
import empty from '../fixtures/empty.json'
import populated from '../fixtures/populated.json'
import untagged from '../fixtures/untagged.json'
import { appState, fakeStore } from '../test/store'
import { pillsOf, summaryOf, TopBar } from './TopBar'

const now = Date.parse('2026-10-01T12:00:05Z')
const words = (s: Parameters<typeof pillsOf>[0]) => pillsOf(s, now).map((p) => `${p.tone} ${p.label}: ${p.value}`)

test('before the first cycle', () => {
  expect(words(appState({ state: empty as unknown as State, conn: 'reconnecting' }))).toEqual([
    'idle Mode: unknown',
    'idle Inventory: unknown',
    'idle Writer: unknown',
    'idle Egress: not checked yet',
    'warn Reconnecting: …',
  ])
})

test('the populated fixture: enforcing, egress off, live for 3 s', () => {
  const s = appState({ state: populated as unknown as State, conn: 'live', times: { at: '2026-10-01T12:00:00Z', finishedAt: '2026-10-01T12:00:02Z', digest: 'd' } })
  expect(words(s)).toEqual(['ok Mode: enforcing', 'ok Inventory: complete', 'ok Writer: ok', 'fail Egress: off', 'ok Live: 3 s'])
  expect(summaryOf(pillsOf(s, now))).toMatchObject({ tone: 'fail', label: 'Egress', value: 'off' })
})

test('observe-only, stale, and the daemon away', () => {
  const st = { ...(untagged as unknown as State), mode: 'observe' }
  expect(words(appState({ state: st, conn: 'stale', connSince: '2026-10-01T11:56:05Z' }))).toEqual([
    'info Mode: observe-only',
    'ok Inventory: complete',
    'ok Writer: ok',
    'ok Egress: on',
    'warn Stale: 4 min',
  ])
  expect(pillsOf(appState({ state: st, conn: 'daemon-down' }), now).at(-1)).toMatchObject({ tone: 'fail', label: 'Daemon', value: 'no answer' })
})

test('all in order: the summary says the mode and live', () => {
  const s = appState({ state: untagged as unknown as State, conn: 'live' })
  expect(summaryOf(pillsOf(s, now))).toMatchObject({ tone: 'ok', label: 'enforcing', value: 'live' })
})

test('the problems, the summary pill and its list, the user menu', async () => {
  const { store } = await fakeStore({ state: populated })
  render(
    <StoreProvider store={store}>
      <TopBar navOpen={false} onNav={() => {}} onAbout={() => {}} />
    </StoreProvider>,
  )
  expect(screen.getByRole('link', { name: '1 problem' }).getAttribute('href')).toBe('/#problems')
  expect(screen.getByText('pve1 · host')).toBeTruthy()
  const summary = screen.getByRole('button', { expanded: false, name: /Egress/ })
  fireEvent.click(summary)
  expect(summary.getAttribute('aria-expanded')).toBe('true')
  const list = document.getElementById(summary.getAttribute('aria-controls') ?? '')
  expect(list?.hidden).toBe(false)
  expect(list?.querySelectorAll('li')).toHaveLength(5)

  const menu = screen.getByRole('button', { name: /user menu/ })
  fireEvent.click(menu)
  expect(screen.getByText('admin · signed in with the Proxmox VE session')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Sign out' })).toBeTruthy()
  fireEvent.keyDown(document, { key: 'Escape' })
  expect(menu.getAttribute('aria-expanded')).toBe('false')
})

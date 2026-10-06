import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import type { ClaimView, State } from '../../api/types.gen'
import claims from '../../fixtures/claims.json'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { renderPage, stubApi, writes } from '../testing'
import { GuestsPage } from './GuestsPage'
import { moveText } from './ResolveDialog'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const www = claims.find((c) => c.hostname === 'www.example.com') as ClaimView

test('the claims are read when the page opens, with every state and the note', async () => {
  const sent = stubApi({ 'GET /api/v1/claims': { status: 200, body: claims } })
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestsPage claims />)
  const table = await screen.findByRole('table', { name: 'Claims' })
  expect(sent.map((s) => `${s.method} ${s.path}`)).toEqual(['GET /api/v1/claims'])
  const rows = within(table).getAllByRole('row').slice(1)
  expect(rows.map((r) => r.querySelector('td[data-label="State"]')?.textContent)).toEqual(['serving', 'pending', 'held', 'unknown', 'conflict'])
  const old = rows.find((r) => r.textContent?.includes('old.example.com'))
  expect(old?.textContent).toContain('no longer asked for since')
  expect(screen.getByRole('link', { name: 'Claims' }).getAttribute('aria-current')).toBe('page')
})

test('hand to: a radio list of the holder and the claimants, the consequence, and the hostname and owner sent', async () => {
  const sent = stubApi({ 'GET /api/v1/claims': { status: 200, body: claims }, 'POST /api/v1/claims/resolve': { status: 200, body: {} } })
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestsPage claims />)
  const table = await screen.findByRole('table', { name: 'Claims' })
  const buttons = within(table).getAllByRole('button', { name: 'Hand to …' })
  expect(buttons).toHaveLength(1)
  fireEvent.click(buttons[0] as HTMLElement)
  const dialog = document.querySelector<HTMLElement>('dialog[open]')
  if (!dialog) throw new Error('no dialog')
  const radios = within(dialog).getAllByRole('radio') as HTMLInputElement[]
  // nobody is picked: the admin chooses, and until then nothing is handed over
  expect(radios.map((r) => [r.value, r.checked])).toEqual([
    ['qemu/101', false],
    ['qemu/102', false],
  ])
  expect(within(dialog).getByRole('button', { name: 'Hand it over' }).hasAttribute('disabled')).toBe(true)
  fireEvent.click(within(dialog).getByRole('radio', { name: /qemu\/102/ }))
  expect(dialog.textContent).toContain(
    'Resolving hands www.example.com to qemu/102 (web-2); qemu/101 (web-1) waits for it from then on, in the place in line its claim gives it.',
  )
  fireEvent.click(within(dialog).getByRole('button', { name: 'Hand it over' }))
  await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/claims/resolve', body: { hostname: 'www.example.com', owner: 'qemu/102' } }]))
  await waitFor(() => expect(sent.filter((s) => s.path === '/api/v1/claims')).toHaveLength(2))
})

test('the holder picked hands nothing over', async () => {
  stubApi({ 'GET /api/v1/claims': { status: 200, body: claims } })
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestsPage claims />)
  fireEvent.click(within(await screen.findByRole('table', { name: 'Claims' })).getByRole('button', { name: 'Hand to …' }))
  const dialog = document.querySelector<HTMLElement>('dialog[open]')
  if (!dialog) throw new Error('no dialog')
  fireEvent.click(within(dialog).getByRole('radio', { name: /qemu\/101/ }))
  expect(within(dialog).getByRole('button', { name: 'Hand it over' }).hasAttribute('disabled')).toBe(true)
})

test('what a hand-over brings about, as far as the state tells', () => {
  const st = populated as unknown as State
  // qemu/102 has a route for www.example.com: it serves it from the next cycle
  expect(moveText(www, 'qemu/102', { ...st, hold: undefined }).outcome).toEqual([
    'From the next cycle qemu/102 holds it, and serves it once its address is verified.',
  ])
  expect(moveText(www, 'qemu/102', st).outcome).toEqual([
    'The daemon holds (no writer identity; run pco setup): the claim moves now, but qemu/102 serves www.example.com only once the daemon stops holding.',
  ])
  const waiting = { ...www, hostname: 'new.example.com' }
  expect(moveText(waiting, 'lxc/201', { ...st, hold: undefined }).outcome).toEqual(['lxc/201 waits for approval: nobody serves new.example.com until it is approved.'])
  expect(moveText(www, 'qemu/999', { ...st, hold: undefined, mode: 'observe' }).outcome).toEqual([
    'qemu/999 names www.example.com without a route for it: nobody serves it until qemu/999 routes it.',
    'The daemon only observes: the claim moves now, but nothing is published until publishing starts.',
  ])
})

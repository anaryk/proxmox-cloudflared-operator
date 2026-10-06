import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import annotation from '../../fixtures/annotation.json'
import approvals from '../../fixtures/approvals.json'
import claims from '../../fixtures/claims.json'
import guests from '../../fixtures/guests.json'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { renderPage, stubApi, writes } from '../testing'
import { GuestDetail } from './GuestDetail'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const reads = {
  'GET /api/v1/guests': { status: 200, body: guests },
  'GET /api/v1/approvals': { status: 200, body: approvals },
  'GET /api/v1/claims': { status: 200, body: claims },
  'GET /api/v1/guests/qemu/101/annotation': { status: 200, body: { ref: 'qemu/101', block: '', startLine: 0, issues: [] } },
  'GET /api/v1/guests/qemu/103/annotation': { status: 200, body: annotation },
}

test('a guest that does not wait is approved from its detail, in the identity shown', async () => {
  const identity = 'uuid:4c4c4544-0042-3510-8051-b4c04f4e3432'
  const sent = stubApi({ ...reads, 'POST /api/v1/guests/approve': { status: 200, body: { owner: 'qemu/101', identity, mode: 'approve' } } })
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestDetail guest="qemu/101" variant="page" />)
  await screen.findByText(identity, { selector: 'dd bdi' })
  const summary = screen.getByRole('region', { name: /^qemu\/101/ })
  fireEvent.click(within(summary).getByRole('button', { name: 'Approve' }))
  const dialog = document.querySelector<HTMLElement>('dialog[open]')
  if (!dialog) throw new Error('no dialog')
  fireEvent.click(within(dialog).getByRole('button', { name: 'Approve' }))
  await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/guests/approve', body: { owner: 'qemu/101', identity } }]))
})

test('its routes, its claims and its approval', async () => {
  stubApi(reads)
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestDetail guest="qemu/101" variant="page" />)
  await screen.findByText('the same')
  const routes = screen.getByRole('table', { name: 'Its routes' })
  expect(within(routes).getByRole('link', { name: 'www.example.com' }).getAttribute('href')).toBe('/routes/www.example.com?owner=qemu%2F101')
  const claimsCard = screen.getByRole('region', { name: 'Claims' })
  await within(claimsCard).findByText('old.example.com')
  expect(claimsCard.textContent).toContain('www.example.com')
  expect(claimsCard.textContent).toContain('it holds it; waiting: qemu/102 (web-2)')
  expect(claimsCard.textContent).toContain('it holds it; nobody waits for it')
  expect(claimsCard.textContent).not.toContain('api.example.com')
  expect(screen.getByRole('button', { name: 'Revoke' })).toBeTruthy()
})

test('the route text of its Notes with the issue marked', async () => {
  const sent = stubApi(reads)
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestDetail guest="qemu/103" variant="drawer" />)
  await screen.findByText('a broken entry', { selector: '.annotation-msg bdi' })
  expect(sent.some((s) => s.path === '/api/v1/guests/qemu/103/annotation')).toBe(true)
  expect(screen.getByRole('heading', { name: 'Notes: 1 issue' })).toBeTruthy()
})

test('a guest that waits says why and what it would publish', async () => {
  stubApi(reads)
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestDetail guest="lxc/202" variant="page" />)
  const summary = await screen.findByRole('region', { name: /^lxc\/202/ })
  expect(summary.textContent).toContain('delegated: alice@pve holds VM.Config.Network; address 10.0.0.1 is the gateway of node pve1')
  expect(summary.textContent).toContain('dns.example.com')
})

test('a guest the listing does not have is not offered for approval', async () => {
  stubApi({ ...reads, 'GET /api/v1/guests/qemu/999/annotation': { status: 404, body: { error: 'qemu/999 is not in the last listing of Proxmox', code: 'not_found' } } })
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestDetail guest="qemu/999" variant="page" />)
  await screen.findByText('The last listing of Proxmox VE does not have this guest.')
  const approve = screen.getByRole('button', { name: 'Approve' })
  expect(approve.getAttribute('aria-disabled')).toBe('true')
  expect(document.getElementById(approve.getAttribute('aria-describedby') ?? '')?.textContent).toBe('it is not in the last listing of Proxmox VE')
})

test('a name that is not a guest asks for nothing', async () => {
  const sent = stubApi(reads)
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <GuestDetail guest="qemu/101/../../x" variant="page" />)
  expect(screen.getByText(/does not name a guest/)).toBeTruthy()
  expect(sent).toEqual([])
})

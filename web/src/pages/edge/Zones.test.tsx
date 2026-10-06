import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { State } from '../../api/types.gen'
import frozen from '../../fixtures/frozen.json'
import populated from '../../fixtures/populated.json'
import settings from '../../fixtures/settings.json'
import { fakeStore, flush } from '../../test/store'
import { renderPage, stubApi, writes } from '../testing'
import { pinsAfter, ZoneDetail } from './ZoneDetail'
import { Zones } from './Zones'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

// The settings that go with the pins of the populated state.
const pinned = { ...settings, settings: { ...settings.settings, zonePins: { 'example.com': 'cred1', 'example.net': 'cred2' } } }

async function open(state: unknown) {
  const { store } = await fakeStore({ state, answers: { 'GET /api/v1/settings': () => ({ status: 200, body: pinned }) } })
  await flush()
  renderPage(store, <Zones />)
  return store
}

function rows() {
  const table = screen.getByRole('table', { name: 'Zones' })
  return within(table)
    .getAllByRole('row')
    .slice(1)
    .map((r) => Object.fromEntries([...r.querySelectorAll('td')].map((td) => [td.dataset.label, td.textContent])))
}

describe('the zones of the last cycle', () => {
  test('every state of the populated fixture, with the account, the credentials and the routes', async () => {
    await open(populated)
    expect(rows().map((r) => [r.Zone, r.State, r.Account, r['Listed by'], r['Served by'], r.Pin, r.Routes, r['Records in the way']])).toEqual([
      ['example.com', 'served', 'Main', 'main, spare', 'main', 'main', '1', '1'],
      ['example.info', 'frozen', 'acc3', 'main', '-', '-', '0', '0'],
      ['example.net', 'not served', 'acc2', 'main, spare', '-', 'spare', '0', '0'],
      ['example.org', 'left out', 'Main', 'main', '-', '-', '0', '0'],
    ])
  })

  test('every state of the frozen fixture, a frozen zone with why in full, and the way to let a stale one go', async () => {
    await open(frozen)
    const st = frozen as unknown as State
    expect(rows().map((r) => [r.Zone, r.State])).toEqual(st.zones.map((z) => [z.name, z.state]).sort())
    expect(new Set(rows().map((r) => r.State))).toEqual(new Set(['served', 'frozen', 'not served', 'left out']))
    const card = screen.getByRole('region', { name: 'Frozen and stale zones' })
    for (const z of st.zones.filter((x) => x.state === 'frozen')) {
      expect(card.textContent).toContain(z.frozenWhy)
    }
    const links = within(card).getAllByRole('link', { name: 'Confirm in the plan' })
    expect(links.map((l) => l.getAttribute('href'))).toEqual(['/routes/plan#waiting-stale-zone-example.info', '/routes/plan#waiting-stale-zone-example.shop'])
  })

  test('a zone left out, with what the credential would need', async () => {
    await open(populated)
    const card = screen.getByRole('region', { name: 'Left out by a credential' })
    const items = within(card).getAllByRole('listitem').map((li) => li.textContent)
    expect(items).toContain('example.org is left out by main (cred1): no DNS readgrant Zone > DNS > Edit on example.org')
  })

  test('before the first cycle', async () => {
    await open({ ...populated, at: '0001-01-01T00:00:00Z', zones: [] })
    expect(screen.getByText('Waiting for the first cycle.')).toBeTruthy()
  })
})

describe('the pin', () => {
  test('is saved with the revision the settings were read at', async () => {
    const sent = stubApi({ 'PUT /api/v1/settings': { status: 200, body: { ...settings, rev: 8, restartNeeded: [] } } })
    await open(populated)
    const row = screen.getAllByRole('row').find((r) => r.querySelector('td')?.textContent === 'example.com')
    fireEvent.click(within(row as HTMLElement).getByRole('button', { name: 'Pin …' }))
    const dialog = document.querySelector<HTMLElement>('dialog[open]')
    if (!dialog) throw new Error('no dialog')
    fireEvent.click(within(dialog).getByRole('radio', { name: /spare/ }))
    expect(dialog.textContent).toContain('zonePins: example.com to spare (cred2), not main (cred1), at revision 7 of the settings.')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save the pin' }))
    await waitFor(() =>
      expect(writes(sent)).toEqual([
        { method: 'PUT', path: '/api/v1/settings', body: { rev: 7, settings: { ...pinned.settings, zonePins: { 'example.com': 'cred2', 'example.net': 'cred2' } } } },
      ]),
    )
  })

  test('settings changed in between are read again, and the next save carries their revision', async () => {
    let saves = 0
    const sent = stubApi({
      'PUT /api/v1/settings': () =>
        ++saves === 1 ? { status: 409, body: { error: 'the settings changed since they were read at revision 7; read them again', code: 'refused' } } : { status: 200, body: settings },
      'GET /api/v1/settings': { status: 200, body: { ...pinned, rev: 9 } },
    })
    await open(populated)
    const row = screen.getAllByRole('row').find((r) => r.querySelector('td')?.textContent === 'example.net')
    fireEvent.click(within(row as HTMLElement).getByRole('button', { name: 'Pin …' }))
    const dialog = document.querySelector<HTMLElement>('dialog[open]')
    if (!dialog) throw new Error('no dialog')
    fireEvent.click(within(dialog).getByRole('radio', { name: /main/ }))
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save the pin' }))
    expect(dialog.textContent).toContain('at revision 7 of the settings')
    fireEvent.click(await within(dialog).findByRole('button', { name: 'Look again' }))
    // what was picked goes with the settings it was picked in
    await waitFor(() => expect((within(dialog).getByRole('radio', { name: /spare/ }) as HTMLInputElement).checked).toBe(true))
    fireEvent.click(within(dialog).getByRole('radio', { name: /main/ }))
    expect(dialog.textContent).toContain('at revision 9 of the settings')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save the pin' }))
    await waitFor(() => expect(writes(sent)).toHaveLength(2))
    expect(writes(sent).map((s) => (s.body as { rev: number }).rev)).toEqual([7, 9])
  })

  test('the pins after a change', () => {
    const s = { ...settings.settings, zonePins: { 'example.com': 'cred1', 'example.net': 'cred2' } }
    expect(pinsAfter(s, 'example.com', 'cred2')).toEqual({ 'example.com': 'cred2', 'example.net': 'cred2' })
    expect(pinsAfter(s, 'example.com', '')).toEqual({ 'example.net': 'cred2' })
    expect(pinsAfter(settings.settings, 'example.org', 'cred1')).toEqual({ 'example.org': 'cred1' })
  })
})

test('the detail of a zone: why it is frozen, the checks of it, its routes', async () => {
  const { store } = await fakeStore({ state: frozen })
  renderPage(store, <ZoneDetail zone="example.shop" variant="drawer" />)
  const st = frozen as unknown as State
  const z = st.zones.find((x) => x.name === 'example.shop')
  expect(screen.getByText(z?.frozenWhy ?? '-')).toBeTruthy()
  expect(screen.getByRole('link', { name: 'Confirm in the plan' }).getAttribute('href')).toBe('/routes/plan#waiting-stale-zone-example.shop')
  const routes = screen.getByRole('table', { name: 'Its routes' })
  expect(within(routes).getByText('cart.example.shop')).toBeTruthy()
  expect(within(routes).getByText('frozen')).toBeTruthy()

  cleanup()
  renderPage(store, <ZoneDetail zone="example.com" variant="page" />)
  const checks = screen.getByRole('region', { name: "What the credentials' checks found" })
  expect(within(checks).getByRole('listitem').textContent).toBe('✗main: dns.read on example.com failedgrant Zone > DNS > Read on example.com')
})

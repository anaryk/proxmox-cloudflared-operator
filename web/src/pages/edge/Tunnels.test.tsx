import { act, cleanup, fireEvent, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { State } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'
import rogue from '../../fixtures/rogue-scenario.json'
import settings from '../../fixtures/settings.json'
import traffic from '../../fixtures/traffic.json'
import { observeOnlyRefusal } from '../../gen/words.gen'
import { fakeStore } from '../../test/store'
import { renderPage, stubApi } from '../testing'
import { TunnelDetail } from './TunnelDetail'
import { Tunnels } from './Tunnels'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

const main = '6ab3311bfe7c52d93098e661558a14c2'
const refused = 'b111e10396903a33c8edf38dbcd9eb8f'
const held = 'b27979bd0d751fe5ef7139c3f73cbd9e'

function rows() {
  return within(screen.getByRole('table', { name: 'Tunnels' }))
    .getAllByRole('row')
    .slice(1)
    .map((r) => Object.fromEntries([...r.querySelectorAll('td')].map((td) => [td.dataset.label, td.textContent])))
}

describe('the list', () => {
  test('tunnels of the same name told apart by the account, with the words of the command line', async () => {
    const { store } = await fakeStore({ state: populated })
    renderPage(store, <Tunnels />)
    expect(rows()).toEqual([
      { Tunnel: 'Main · 00000000', Name: 'pco-abc123', Verified: 'yes', Rollout: 'version 3 runs on 1 connector', Connector: 'active, ready, 4 connections', 'Not run by pco': '1' },
      { Tunnel: 'acc2 · -', Name: 'pco-abc123', Verified: 'unknown', Rollout: '-', Connector: 'none', 'Not run by pco': '0' },
      {
        Tunnel: 'acc3 · 00000000',
        Name: 'pco-abc123',
        Verified: 'held: account frozen: zone example.info is no longer listed by credential cred1',
        Rollout: '-',
        Connector: 'none',
        'Not run by pco': '0',
      },
      { Tunnel: 'acc4 · 00000000', Name: 'pco-abc123', Verified: 'unchecked: no writer identity; run pco setup', Rollout: '-', Connector: 'none', 'Not run by pco': '0' },
    ])
    const links = screen.getAllByRole('link').filter((a) => a.getAttribute('href')?.startsWith('/edge/tunnels/'))
    expect(links.map((a) => a.getAttribute('href'))).toEqual(['/edge/tunnels/acc1', '/edge/tunnels/acc2', '/edge/tunnels/acc3', '/edge/tunnels/acc4'])
  })

  test('a tunnel no credential sees waits for the confirmation of the plan', async () => {
    const { store } = await fakeStore({ state: populated })
    renderPage(store, <Tunnels />)
    const card = screen.getByRole('region', { name: 'Tunnels no credential sees' })
    expect(card.textContent).toContain('tunnel pco-abc123 (00000000-0000-4000-8000-000000000002) in account acc2 is not visible through any credential')
    expect(within(card).getByRole('link', { name: 'Confirm in the plan' }).getAttribute('href')).toBe('/routes/plan#waiting-unseen-tunnel-pco-abc123')
  })

  test('the flags of the connectors', async () => {
    const { store } = await fakeStore({ state: rogue })
    renderPage(store, <Tunnels />)
    const connectors = Object.fromEntries(rows().map((r) => [r.Tunnel, r.Connector]))
    expect(connectors).toMatchObject({
      'Main · 00000000': 'active, ready, 4 connections',
      [`${refused} · 00000000`]: 'active, not ready, Cloudflare refuses its token',
      [`${held} · 00000000`]: 'active, not ready, its metrics port is held by another process',
    })
  })
})

describe('the detail', () => {
  test('the connectors pco does not run, in a red block with the command for root and nothing sent', async () => {
    const sent = stubApi({})
    const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    const { store } = await fakeStore({ state: rogue })
    renderPage(store, <TunnelDetail accountId={main} variant="page" />)
    const block = screen.getByRole('region', { name: 'Connectors pco does not run' })
    expect(block.className).toBe('rogue')
    const items = within(block).getAllByRole('listitem').map((li) => li.textContent)
    expect(items).toHaveLength(2)
    expect(items[0]).toMatch(/^0d5e9a77-3b1c-4f2e-8a6d-5c4b3a291807 from 198\.51\.100\.7 \(cloudflared 2026\.8\.0\), first seen /)
    expect(items[1]).toMatch(/^4f8a2c19-7e3d-4b6a-9c05-1d2e3f405162 from 203\.0\.113\.24 \(cloudflared 2025\.11\.1\), first seen /)
    expect(within(block).getByText(`pco tunnel rotate --account ${main}`).tagName).toBe('CODE')
    expect(block.textContent).toContain('Run it as root on the node.')
    expect(within(block).queryByText(observeOnlyRefusal, { exact: false })).toBeNull()
    await act(async () => {
      fireEvent.click(within(block).getByRole('button', { name: 'Copy' }))
    })
    expect(write).toHaveBeenCalledWith(`pco tunnel rotate --account ${main}`)
    expect(within(block).getAllByRole('button').map((b) => b.textContent)).toEqual(['Copy'])
    expect(sent).toEqual([])
  })

  test('a refused token shows the same command, and the connector its flag and id', async () => {
    const { store } = await fakeStore({ state: rogue })
    renderPage(store, <TunnelDetail accountId={refused} variant="drawer" />)
    const block = screen.getByRole('region', { name: 'Connectors pco does not run' })
    expect(block.textContent).toContain('Cloudflare refuses the token of its connector')
    expect(within(block).getByText(`pco tunnel rotate --account ${refused}`)).toBeTruthy()
    expect(screen.getByRole('region', { name: 'Connector' }).textContent).toContain('active, not ready, Cloudflare refuses its token')
  })

  test('a tunnel without connectors pco does not run, and a token Cloudflare takes, has no block', async () => {
    const { store } = await fakeStore({ state: rogue })
    renderPage(store, <TunnelDetail accountId={held} variant="page" />)
    expect(screen.queryByRole('region', { name: 'Connectors pco does not run' })).toBeNull()
    expect(screen.getByRole('region', { name: 'Connector' }).textContent).toContain('its metrics port is held by another process')
  })

  test('an account id of another form gives no command and no copy button', async () => {
    const odd = 'acc1; reboot'
    const st = rogue as unknown as State
    const changed = {
      ...st,
      tunnels: st.tunnels.map((t) => (t.accountId === main ? { ...t, accountId: odd } : t)),
      rogueConnectors: st.rogueConnectors.map((r) => ({ ...r, accountId: odd })),
    }
    const { store } = await fakeStore({ state: changed })
    renderPage(store, <TunnelDetail accountId={odd} variant="page" />)
    const block = screen.getByRole('region', { name: 'Connectors pco does not run' })
    expect(block.textContent).toContain('No command to copy: the account id has an unexpected form')
    expect(within(block).queryByRole('button', { name: 'Copy' })).toBeNull()
    expect(block.querySelector('code')).toBeNull()
  })

  test('in observe-only mode the command comes with the daemon’s refusal', async () => {
    const { store } = await fakeStore({
      state: rogue,
      answers: { 'GET /api/v1/settings': () => ({ status: 200, body: { ...settings, settings: { ...settings.settings, observeOnly: true } } }) },
    })
    renderPage(store, <TunnelDetail accountId={main} variant="page" />)
    const block = screen.getByRole('region', { name: 'Connectors pco does not run' })
    expect(within(block).getByText(`pco tunnel rotate --account ${main}`)).toBeTruthy()
    expect(block.textContent).toContain(`The daemon refuses the rotation for now: ${observeOnlyRefusal}.`)
  })

  test('the tunnel of a frozen account gives the daemon’s reason instead of the command', async () => {
    const st = rogue as unknown as State
    const frozen = { ...st, tunnels: st.tunnels.map((t) => (t.accountId === main ? { ...t, held: 'account frozen: as the test has it', leftAsIs: true } : t)) }
    const { store } = await fakeStore({ state: frozen })
    renderPage(store, <TunnelDetail accountId={main} variant="page" />)
    const block = screen.getByRole('region', { name: 'Connectors pco does not run' })
    expect(block.textContent).toContain(`refused: tunnel pco-abc123 in account ${main} is left as it is: account frozen: as the test has it; nothing was changed`)
    expect(block.querySelector('code')).toBeNull()
  })

  test('the fields, the traffic chart of requests and errors, and the events of the account', async () => {
    const { store } = await fakeStore({ state: populated, answers: { 'GET /api/v1/traffic': () => ({ status: 200, body: traffic }) } })
    renderPage(store, <TunnelDetail accountId="acc1" variant="page" />)
    const summary = screen.getByRole('region', { name: 'Main · 00000000' })
    expect(summary.textContent).toContain('version 3 runs on 1 connector')
    expect(summary.textContent).toContain('00000000-0000-4000-8000-000000000001')
    const connector = screen.getByRole('region', { name: 'Connector' })
    expect(connector.textContent).toContain('6b1f0e4c-29a4-4c43-9d2c-0f3a8c1b7d11')
    expect(connector.textContent).toContain('2026.9.3')
    expect(connector.textContent).toContain('fra08, prg01')
    expect(within(connector).getByRole('img', { name: 'Requests and errors' })).toBeTruthy()
    const events = within(screen.getByRole('region', { name: 'Events' })).getByRole('table', { name: 'Events' })
    expect(within(events).getAllByRole('row').length).toBeGreaterThan(1)
    expect(within(events).queryByText('adoption requested for the next run')).toBeNull()
  })
})

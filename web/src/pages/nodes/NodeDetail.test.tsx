import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { Event, State } from '../../api/types.gen'
import cluster from '../../fixtures/cluster.json'
import { fakeStore } from '../../test/store'
import { renderPage, stubApi, writes } from '../testing'
import { NodeDetail, routesOn } from './NodeDetail'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const st = cluster as unknown as State

const dialog = () => {
  const d = document.querySelector<HTMLElement>('dialog[open]')
  if (!d) throw new Error('no dialog is open')
  return d
}

const nodeView = (name: string) => {
  const n = st.nodes.find((x) => x.name === name)
  if (!n) throw new Error(`no node ${name}`)
  return n
}

describe('the detail of a node', () => {
  test('what its agent reported, and the routes it has a part in', async () => {
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve2" variant="page" />)
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('pve2')
    const text = document.body.textContent ?? ''
    expect(text).toContain('heard 4s ago, watching')
    expect(text).toContain('build 1.4.0, protocol 1')
    expect(text).toContain('10.10.0.2')
    expect(text).toContain('ready: its shared connector runs')
    expect(text).toContain('10 verified · 1 failing · 1 pinned')
    expect(text).toContain('+300 ms (ahead)')
    const pinned = screen.getByRole('list', { name: 'Routes pinned to it' })
    expect(within(pinned).getByRole('link', { name: 'git.example.com' }).getAttribute('href')).toBe('/routes/git.example.com?owner=qemu%2F110')
    expect(pinned.textContent).toContain('reached, in its set')
    const replica = screen.getByRole('list', { name: 'Routes it is a replica of' })
    expect(replica.textContent).toContain('www.example.com')
    expect(replica.textContent).toContain('port (attested by pve1)')
    expect(replica.textContent).toContain('monitor.example.com')
    expect(replica.textContent).toContain('no answer on port 8443 from 10.10.0.2')
  })

  test('routesOn: pinned to it, or a replica of', () => {
    const on = routesOn(st, 'pve2')
    expect(on.pinned.map((r) => r.hostname)).toEqual(['git.example.com'])
    expect(on.replica.map((r) => r.hostname)).toContain('www.example.com')
    expect(on.replica.map((r) => r.hostname)).not.toContain('git.example.com')
    expect(routesOn(st, 'pve3')).toEqual({ pinned: [], replica: [] })
  })

  test('the events the stream brought of the node', async () => {
    const events: Event[] = [
      { seq: 1, at: '2026-10-01T11:58:00Z', level: 'info', kind: 'enrol', subject: 'pve3', message: 'node pve3 enrolled with its join token', node: 'pve3' },
      { seq: 2, at: '2026-10-01T11:59:00Z', level: 'info', kind: 'node', subject: 'pve2', message: 'node pve2 is active', node: 'pve2' },
    ]
    const { store } = await fakeStore({ state: st, events })
    renderPage(store, <NodeDetail name="pve3" variant="drawer" />)
    const list = screen.getByRole('list', { name: 'Events of the node' })
    expect(list.textContent).toContain('node pve3 enrolled with its join token')
    expect(list.textContent).not.toContain('node pve2 is active')
  })

  test('a node that is not in the registry', async () => {
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve9" variant="page" />)
    expect(screen.getByText('No node of this name is in the registry')).toBeTruthy()
  })
})

describe('admitting an appliance agent', () => {
  test('the dialog shows every use of the join token and refuses a wrong fingerprint before anything is sent', async () => {
    const sent = stubApi({})
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve3" variant="page" />)
    fireEvent.click(screen.getByRole('button', { name: 'Admit…' }))
    const d = dialog()
    expect(d.textContent).toContain('waits for admission')
    const uses = within(d).getByRole('list', { name: 'Uses of the join token' })
    expect(uses.textContent).toContain('from 10.0.0.93, as container 9301, with MAC bc:24:11:00:93:01')
    const field = within(d).getByRole('textbox', { name: 'Fingerprint of the certificate' })
    fireEvent.click(within(d).getByRole('button', { name: 'Admit' }))
    expect(d.textContent).toContain('an appliance agent is admitted with the fingerprint of its certificate')
    fireEvent.change(field, { target: { value: 'zz' } })
    fireEvent.click(within(d).getByRole('button', { name: 'Admit' }))
    expect(d.textContent).toContain('digits 0-9 and a-f only')
    fireEvent.change(field, { target: { value: 'abc' } })
    fireEvent.click(within(d).getByRole('button', { name: 'Admit' }))
    expect(d.textContent).toContain('at least 8 hex digits')
    expect(writes(sent)).toEqual([])
  })

  test('a fingerprint of the right form is sent as typed, in lower case without colons, and the daemon’s refusal is shown', async () => {
    const refusal = 'the fingerprint 0123abcd is not that of the certificate enrolled for node pve3 (deadbeef); read it with pco agent status on the node'
    const sent = stubApi({
      'POST /api/v1/nodes/pve3/admit': (s) =>
        (s.body as { fingerprint: string }).fingerprint === 'deadbeef' ? { status: 200, body: { ...nodeView('pve3'), state: 'active' } } : { status: 409, body: { code: 'refused', error: refusal } },
    })
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve3" variant="page" />)
    fireEvent.click(screen.getByRole('button', { name: 'Admit…' }))
    const field = within(dialog()).getByRole('textbox', { name: 'Fingerprint of the certificate' })
    fireEvent.change(field, { target: { value: '01:23:AB:CD' } })
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Admit' }))
    await within(dialog()).findByText(refusal)
    fireEvent.change(field, { target: { value: 'DE:AD:BE:EF' } })
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Admit' }))
    await waitFor(() =>
      expect(writes(sent)).toEqual([
        { method: 'POST', path: '/api/v1/nodes/pve3/admit', body: { fingerprint: '0123abcd', renew: false } },
        { method: 'POST', path: '/api/v1/nodes/pve3/admit', body: { fingerprint: 'deadbeef', renew: false } },
      ]),
    )
  })

  test('a host node needs no fingerprint', async () => {
    const host: State = { ...st, nodes: st.nodes.map((n) => (n.name === 'pve3' ? { ...n, kind: 'host', vmid: undefined, joinUses: undefined } : n)) }
    const sent = stubApi({ 'POST /api/v1/nodes/pve3/admit': { status: 200, body: { ...nodeView('pve3'), state: 'active' } } })
    const { store } = await fakeStore({ state: host })
    renderPage(store, <NodeDetail name="pve3" variant="page" />)
    fireEvent.click(screen.getByRole('button', { name: 'Admit…' }))
    expect(dialog().textContent).toContain('no fingerprint is needed')
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Admit' }))
    await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/nodes/pve3/admit', body: { fingerprint: '', renew: false } }]))
  })
})

describe('draining and forgetting', () => {
  test('a drain, with force for the last replica, and the end of a drain', async () => {
    const sent = stubApi({
      'POST /api/v1/nodes/pve2/drain': { status: 200, body: { ...nodeView('pve2'), state: 'draining', drained: 'admin' } },
      'POST /api/v1/nodes/pve2/undrain': { status: 200, body: nodeView('pve2') },
    })
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve2" variant="page" />)
    fireEvent.click(screen.getByRole('button', { name: 'Drain…' }))
    expect(dialog().textContent).toContain('takes it out of the shared tunnel')
    fireEvent.click(within(dialog()).getByRole('checkbox'))
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Drain' }))
    await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/nodes/pve2/drain', body: { force: true } }]))
    cleanup()

    const drained: State = { ...st, nodes: st.nodes.map((n) => (n.name === 'pve2' ? { ...n, state: 'draining', drained: 'admin' } : n)) }
    const second = await fakeStore({ state: drained })
    renderPage(second.store, <NodeDetail name="pve2" variant="page" />)
    expect(screen.queryByRole('button', { name: 'Drain…' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'End the drain…' }))
    fireEvent.click(within(dialog()).getByRole('button', { name: 'End the drain' }))
    await waitFor(() => expect(writes(sent)).toHaveLength(2))
    expect(writes(sent)[1]).toEqual({ method: 'POST', path: '/api/v1/nodes/pve2/undrain', body: {} })
  })

  test('a drain of maintenance mode is ended in Proxmox, not here', async () => {
    const maintenance: State = { ...st, nodes: st.nodes.map((n) => (n.name === 'pve2' ? { ...n, state: 'draining', drained: 'maintenance' } : n)) }
    const { store } = await fakeStore({ state: maintenance })
    renderPage(store, <NodeDetail name="pve2" variant="page" />)
    fireEvent.click(screen.getByRole('button', { name: 'End the drain…' }))
    expect(within(dialog()).getByRole('button', { name: 'End the drain' }).getAttribute('aria-disabled')).toBe('true')
    expect(dialog().textContent).toContain('ha-manager crm-command node-maintenance disable')
  })

  test('forgetting a node needs its name typed, says what was done, and gives the command of a compromised node to root', async () => {
    const result = {
      node: 'pve2',
      compromised: false,
      done: ['the entry of the node is removed: its agent is refused from its next request', 'its connections to this controller are closed'],
      pending: ['the secret of the shared tunnel is rotated once pco apply ends observe-only mode'],
    }
    const sent = stubApi({ 'POST /api/v1/nodes/pve2/forget': { status: 200, body: result } })
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve2" variant="page" />)
    fireEvent.click(screen.getByRole('button', { name: 'Forget…' }))
    const d = dialog()
    expect(d.textContent).toContain('the secret of the shared tunnel is rotated')
    expect(within(d).getByText('pvecm delnode pve2').tagName).toBe('CODE')
    expect(within(d).getByText('pco node forget pve2 --compromised').tagName).toBe('CODE')
    const forget = within(d).getByRole('button', { name: 'Forget' })
    expect(forget.getAttribute('aria-disabled')).toBe('true')
    fireEvent.click(forget)
    expect(writes(sent)).toEqual([])
    fireEvent.change(within(d).getByRole('textbox', { name: 'Type the name of the node to confirm' }), { target: { value: 'pve2' } })
    fireEvent.click(within(d).getByRole('button', { name: 'Forget' }))
    await within(d).findByRole('list', { name: 'Done' })
    expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/nodes/pve2/forget', body: { compromised: false } }])
    expect(within(d).getByRole('list', { name: 'Done' }).textContent).toContain('its connections to this controller are closed')
    expect(within(d).getByRole('list', { name: 'Pending' }).textContent).toContain('once pco apply ends observe-only mode')
  })

  test('the daemon’s refusal of a forget is shown as it is, with the pvecm delnode line it gives', async () => {
    const refusal = 'run pvecm delnode pve2 first: a node still in the cluster would receive every new secret'
    stubApi({ 'POST /api/v1/nodes/pve2/forget': { status: 409, body: { code: 'refused', error: refusal } } })
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve2" variant="page" />)
    fireEvent.click(screen.getByRole('button', { name: 'Forget…' }))
    fireEvent.change(within(dialog()).getByRole('textbox', { name: 'Type the name of the node to confirm' }), { target: { value: 'pve2' } })
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Forget' }))
    await within(dialog()).findByText(refusal)
  })

  test('the controller is not forgotten here, and a reader does nothing', async () => {
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodeDetail name="pve1" variant="page" />)
    const forget = screen.getByRole('button', { name: 'Forget…' })
    expect(forget.getAttribute('aria-disabled')).toBe('true')
    expect(forget.parentElement?.textContent).toContain('pco uninstall')
    cleanup()
    const reader = await fakeStore({ state: st, session: { role: 'reader' } })
    renderPage(reader.store, <NodeDetail name="pve3" variant="page" />)
    for (const name of ['Admit…', 'Drain…', 'Forget…']) {
      const button = screen.getByRole('button', { name })
      expect(button.getAttribute('aria-disabled')).toBe('true')
      expect(button.parentElement?.textContent).toContain('needs Sys.Modify on /')
    }
  })
})

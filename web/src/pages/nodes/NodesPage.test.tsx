import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { State } from '../../api/types.gen'
import cluster from '../../fixtures/cluster.json'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { renderPage, stubApi, writes } from '../testing'
import { NodesPage } from './NodesPage'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

const st = cluster as unknown as State

function rows() {
  return within(screen.getByRole('table', { name: 'Nodes' }))
    .getAllByRole('row')
    .slice(1)
    .map((r) => Object.fromEntries([...r.querySelectorAll('td')].map((td) => [td.dataset.label, td.textContent])))
}

const dialog = () => {
  const d = document.querySelector<HTMLElement>('dialog[open]')
  if (!d) throw new Error('no dialog is open')
  return d
}

describe('the nodes of the cluster scenario', () => {
  test('a row for each node in the order of pco node list, with what the controller heard of it', async () => {
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    const list = rows()
    expect(list.map((r) => r.Node?.slice(0, 4))).toEqual(['pve1', 'pve2', 'pve3'])
    expect(list[0]).toMatchObject({ State: 'active', Role: 'controller', Agent: 'heard 1s ago, watching', Replica: 'ready', Targets: '9 verified · 1 failing', 'Clock skew': 'none' })
    expect(list[0]?.Node).toContain('controller')
    expect(list[0]?.Node).toContain('host · 1.4.0')
    expect(list[1]).toMatchObject({ State: 'active', Role: 'agent', Agent: 'heard 4s ago, watching', Targets: '10 verified · 1 failing · 1 pinned', 'Clock skew': '+300 ms (ahead)' })
    expect(list[2]).toMatchObject({ State: 'joining', Role: 'agent', Agent: 'heard 4s ago', Replica: 'not ready', Targets: '0 verified' })
    expect(list[2]?.Node).toContain('appliance, container 9301')
    expect(screen.getAllByRole('link', { name: 'pve3' })[0]?.getAttribute('href')).toBe('/nodes/pve3')
  })

  test('the cluster card: the controller and its lease, the members, quorum and coverage', async () => {
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    const card = screen.getByRole('region', { name: 'Cluster' })
    expect(card.textContent).toContain('pve1')
    expect(card.textContent).toContain('generation 7')
    expect(card.textContent).toContain('valid')
    expect(card.textContent).toContain('refreshed 3 s ago')
    expect(card.textContent).toContain('Held by pve2 before.')
    expect(card.textContent).toContain('pve1, pve2, pve3')
    expect(card.textContent).toContain('quorate')
    expect(card.textContent).toContain('every node watches')
  })

  test('the coverage of an appliance install is pinned by design, and not lost', async () => {
    const cluster = st.cluster && { ...st.cluster, coverage: { covered: false, uncovered: ['pve1', 'pve2'], pinned: true } }
    const { store } = await fakeStore({ state: { ...st, cluster } })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    const card = screen.getByRole('region', { name: 'Cluster' })
    expect(card.textContent).toContain("The appliances watch no forwarding tables: the routes of each node's guests are served from it.")
    expect(card.textContent).not.toContain('lost')
    expect(card.textContent).not.toContain('Without a running watch')
  })

  test('the blocked address and the dialog that ends its block', async () => {
    const sent = stubApi({ 'POST /api/v1/cluster/unblock': { status: 200, body: {} } })
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    const card = screen.getByRole('region', { name: 'Blocked addresses' })
    expect(card.textContent).toContain('10.0.0.60')
    expect(card.textContent).toContain('tap140i0 of pve2')
    fireEvent.click(within(card).getByRole('button', { name: 'End the block…' }))
    expect(dialog().textContent).toContain('is blocked on every node')
    fireEvent.click(within(dialog()).getByRole('button', { name: 'End the block' }))
    await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/cluster/unblock', body: { address: '10.0.0.60' } }]))
  })

  test('a join token is made for a named node and shown once', async () => {
    const token = { id: 'abcd1234', node: 'pve4', expires: '2026-10-01T12:15:00Z', token: 'pco1.eyJ2IjoxfQ' }
    const sent = stubApi({ 'POST /api/v1/cluster/join-tokens': { status: 201, body: token } })
    const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Join token…' }))
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Make the token' }))
    expect(dialog().textContent).toContain('a node is named')
    expect(writes(sent)).toEqual([])
    fireEvent.change(within(dialog()).getByRole('textbox', { name: 'Node the agent runs on' }), { target: { value: 'PVE4' } })
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Make the token' }))
    expect(dialog().textContent).toContain('lower-case letters')
    fireEvent.change(within(dialog()).getByRole('textbox', { name: 'Node the agent runs on' }), { target: { value: 'pve4' } })
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Make the token' }))
    await within(dialog()).findByText('pco1.eyJ2IjoxfQ')
    expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/cluster/join-tokens', body: { node: 'pve4' } }])
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Copy' }))
    await waitFor(() => expect(write).toHaveBeenCalledWith('pco1.eyJ2IjoxfQ'))
  })

  test('the lease is handed off to an active node that is not the controller', async () => {
    const sent = stubApi({ 'POST /api/v1/cluster/handoff': { status: 200, body: { from: 'pve1', to: 'pve2', until: '2026-10-01T12:00:32Z' } } })
    const { store } = await fakeStore({ state: st })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Hand off the lease…' }))
    const choices = [...within(dialog()).getByRole('combobox', { name: 'Hand it to' }).querySelectorAll('option')].map((o) => o.value)
    expect(choices).toEqual(['', 'pve2'])
    fireEvent.change(within(dialog()).getByRole('combobox', { name: 'Hand it to' }), { target: { value: 'pve2' } })
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Hand off' }))
    await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/cluster/handoff', body: { to: 'pve2' } }]))
  })

  test('a node offered for the lease is named as untrusted text', async () => {
    stubApi({})
    const odd: State = { ...st, nodes: st.nodes.map((n) => (n.name === 'pve2' ? { ...n, name: 'pve2\u202e1evp' } : n)) }
    const { store } = await fakeStore({ state: odd })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Hand off the lease…' }))
    const options = [...within(dialog()).getByRole('combobox', { name: 'Hand it to' }).querySelectorAll('option')]
    expect(options.map((o) => o.value)).toEqual(['', 'pve2\u202e1evp'])
    expect(options[1]?.textContent).toMatch(/^pve2.+1evp$/)
    expect(options[1]?.textContent).not.toContain('\u202e')
  })

  test('a reader sees the actions disabled, with the privilege they need', async () => {
    const { store } = await fakeStore({ state: st, session: { role: 'reader' } })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    for (const name of ['Join token…', 'Hand off the lease…']) {
      const button = screen.getByRole('button', { name })
      expect(button.getAttribute('aria-disabled')).toBe('true')
      expect(button.parentElement?.textContent).toContain('needs Sys.Modify on /')
    }
  })
})

describe('a node alone', () => {
  test('says so, with no row and nothing to join', async () => {
    const { store } = await fakeStore({ state: populated })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    expect(screen.getByText('This node stands alone.')).toBeTruthy()
    expect(screen.queryByRole('table', { name: 'Nodes' })).toBeNull()
    expect(screen.queryByRole('region', { name: 'Cluster' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Join token…' }).getAttribute('aria-disabled')).toBe('true')
  })

  test('a node that does not hold the lease names the controller and offers its own agent', async () => {
    const other: State = { ...(populated as unknown as State), lease: { holder: 'pve2', address: '10.10.0.2', generation: 7, refreshAge: 0, valid: false, acquiredAt: '2026-10-01T11:00:00Z' } }
    const { store } = await fakeStore({ state: other })
    renderPage(store, <NodesPage view={{ name: 'nodes' }} />)
    const card = screen.getByRole('region', { name: 'This node is not the controller' })
    expect(card.textContent).toContain('Node pve2 holds the lease')
    expect(within(card).getByRole('link', { name: "This node's agent" }).getAttribute('href')).toBe('/agent')
  })
})

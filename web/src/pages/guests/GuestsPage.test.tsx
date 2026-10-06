import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { State } from '../../api/types.gen'
import approvals from '../../fixtures/approvals.json'
import guests from '../../fixtures/guests.json'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { readerReason } from '../kit'
import { renderPage, type Sent, stubApi, writes } from '../testing'
import { incompleteReason } from './ApproveDialog'
import { GuestsPage } from './GuestsPage'

// The page goes before the stub of fetch does, so that nothing it reads
// after the test reaches the network of the DOM.
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const reads = {
  'GET /api/v1/guests': { status: 200, body: guests },
  'GET /api/v1/approvals': { status: 200, body: approvals },
}

async function open(state: unknown = populated, more: Parameters<typeof stubApi>[0] = {}, role = 'admin') {
  const sent = stubApi({ ...reads, ...more })
  const { store } = await fakeStore({ state, session: { role } })
  renderPage(store, <GuestsPage />)
  await screen.findByRole('table', { name: 'All tagged guests' })
  return sent
}

const table = (name: string) => screen.getByRole('table', { name })
const rowOf = (name: string, text: string) => {
  const row = within(table(name))
    .getAllByRole('row')
    .find((r) => r.textContent?.includes(text))
  if (!row) throw new Error(`no row with ${text} in ${name}`)
  return row
}
const dialog = () => {
  const d = document.querySelector<HTMLElement>('dialog[open]')
  if (!d) throw new Error('no dialog is open')
  return d
}

describe('waiting for approval', () => {
  test('each guest with the identity and the hostnames an approval is of', async () => {
    await open()
    const row = rowOf('Guests waiting for approval', 'lxc/202')
    expect(row.textContent).toContain('dns-1')
    expect(row.textContent).toContain('uuid:202')
    expect(row.textContent).toContain('dns.example.com')
    expect(row.textContent).toContain('address 10.0.0.1 is the gateway of node pve1')
  })

  test('approve sends the identity, the MACs and the addresses the dialog shows', async () => {
    const sent = await open(populated, { 'POST /api/v1/guests/approve': { status: 200, body: { owner: 'lxc/202', identity: 'uuid:202', mode: 'approve' } } })
    fireEvent.click(within(rowOf('Guests waiting for approval', 'lxc/202')).getByRole('button', { name: 'Approve' }))
    const d = dialog()
    expect(d.textContent).toContain('waits for approval in identity uuid:202; approved, it publishes dns.example.com.')
    expect(d.textContent).toContain('Approving it records MAC bc:24:11:00:02:02 and allows address 10.0.0.1.')
    fireEvent.click(within(d).getByRole('button', { name: 'Approve' }))
    await waitFor(() =>
      expect(writes(sent)).toEqual([
        { method: 'POST', path: '/api/v1/guests/approve', body: { owner: 'lxc/202', identity: 'uuid:202', macs: ['bc:24:11:00:02:02'], addresses: ['10.0.0.1'] } },
      ]),
    )
    expect(await screen.findByText(/^Approved lxc\/202 in identity uuid:202\. From the next cycle/)).toBeTruthy()
    expect(document.querySelector('dialog[open]')).toBeNull()
  })

  test('a guest without MACs to record is approved by its identity alone', async () => {
    const sent = await open(populated, { 'POST /api/v1/guests/approve': { status: 200, body: { owner: 'lxc/201', identity: 'uuid:201', mode: 'approve' } } })
    fireEvent.click(within(rowOf('Guests waiting for approval', 'lxc/201')).getByRole('button', { name: 'Approve' }))
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/guests/approve', body: { owner: 'lxc/201', identity: 'uuid:201' } }]))
  })

  test('refused: the dialog stays with the daemon’s sentence and Look again reads the lists again', async () => {
    const why = 'lxc/201 changed since it was shown: it was shown in identity uuid:201 and has identity uuid:999 now; look at it again'
    const sent = await open(populated, { 'POST /api/v1/guests/approve': { status: 409, body: { error: why, code: 'refused' } } })
    fireEvent.click(within(rowOf('Guests waiting for approval', 'lxc/201')).getByRole('button', { name: 'Approve' }))
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Approve' }))
    const alert = await within(dialog()).findByRole('alert')
    expect(alert.textContent).toContain(why)
    const reads = (s: Sent[]) => s.filter((x) => x.method === 'GET').length
    const before = reads(sent)
    fireEvent.click(within(alert).getByRole('button', { name: 'Look again' }))
    await waitFor(() => expect(reads(sent)).toBe(before + 2))
    expect(document.querySelector('dialog[open]')).toBeNull()
  })

  test('approve is disabled with the daemon’s reason while the last cycle did not list every guest', async () => {
    const sent = await open({ ...populated, complete: false })
    const button = within(rowOf('Guests waiting for approval', 'lxc/201')).getByRole('button', { name: 'Approve' })
    expect(button.getAttribute('aria-disabled')).toBe('true')
    expect(document.getElementById(button.getAttribute('aria-describedby') ?? '')?.textContent).toBe(incompleteReason)
    fireEvent.click(button)
    expect(document.querySelector('dialog[open]')).toBeNull()
    expect(writes(sent)).toEqual([])
  })

  test('a reader is told what an approval needs', async () => {
    await open(populated, {}, 'reader')
    const button = within(rowOf('Guests waiting for approval', 'lxc/201')).getByRole('button', { name: 'Approve' })
    expect(document.getElementById(button.getAttribute('aria-describedby') ?? '')?.textContent).toBe(readerReason)
  })
})

describe('all tagged guests', () => {
  test('a guest that does not wait is approved in the identity the listing shows', async () => {
    const identity = 'uuid:4c4c4544-0042-3510-8051-b4c04f4e3432'
    const sent = await open(populated, { 'POST /api/v1/guests/approve': { status: 200, body: { owner: 'qemu/101', identity, mode: 'tag' } } })
    expect(within(table('All tagged guests')).queryByText('lxc/200')).toBeNull()
    fireEvent.click(within(rowOf('All tagged guests', 'qemu/101')).getByRole('button', { name: 'Approve' }))
    expect(dialog().textContent).toContain(`has identity ${identity} in the last listing`)
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/guests/approve', body: { owner: 'qemu/101', identity } }]))
    expect(await screen.findByText(/The admission mode is tag: the approval matters only for its routes at observed/)).toBeTruthy()
  })

  test('after a cycle that did not list every guest the list says so', async () => {
    await open({ ...populated, complete: false }, { 'GET /api/v1/guests': { status: 200, body: [] } })
    expect(within(table('All tagged guests')).getByText('The last cycle did not list every guest.')).toBeTruthy()
  })
})

describe('approved', () => {
  test('with the identity now, and revoke sends the owner', async () => {
    const sent = await open(populated, { 'POST /api/v1/guests/revoke': { status: 200, body: {} } })
    await screen.findByRole('table', { name: 'Approved guests' })
    expect(rowOf('Approved guests', 'qemu/101').textContent).toContain('the same')
    expect(rowOf('Approved guests', 'lxc/300').textContent).toContain('not in the last listing')
    fireEvent.click(within(rowOf('Approved guests', 'lxc/300')).getByRole('button', { name: 'Revoke' }))
    expect(dialog().textContent).toContain('Revoking it stops its routes from being published while the admission mode is approve.')
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Revoke' }))
    await waitFor(() => expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/guests/revoke', body: { owner: 'lxc/300' } }]))
  })
})

describe('annotation issues', () => {
  test('only those of guests, and not the one every waiting guest has', async () => {
    const st = populated as unknown as State
    await open({
      ...st,
      issues: [...st.issues, { guest: { kind: 'lxc', vmid: 201 }, msg: 'waiting for approval' }, { guest: { kind: 'lxc', vmid: 202 }, msg: 'waiting for approval' }],
    })
    const rows = within(table('Annotation issues')).getAllByRole('row').slice(1)
    expect(rows.map((r) => [...r.querySelectorAll('td')].map((td) => td.textContent))).toEqual([['qemu/103', '2', '5', 'a broken entry']])
    expect(within(rows[0] as HTMLElement).getByRole('link', { name: 'qemu/103' }).getAttribute('href')).toBe('/guests/qemu/103')
  })
})

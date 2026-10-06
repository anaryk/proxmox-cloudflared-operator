import { act, cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { CredentialView, Report, State } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { renderPage, type Reply, stubApi, writes } from '../testing'
import { CredentialDetail } from './CredentialDetail'
import { Credentials } from './Credentials'
import { deepLimitText, deepQuestion } from './DeepCheckDialog'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const st = populated as unknown as State
const main = st.credentials[0] as CredentialView
const spare = st.credentials[1] as CredentialView
const report = main.report as Report

const zones = (n: number) => Array.from({ length: n }, (_, i) => ({ id: `zone${i}`, name: `z${i}.example.com`, status: 'active', accountId: 'acc1' }))
const wide: CredentialView = { ...main, report: { ...report, zones: zones(13) } }

async function open(list: CredentialView[], more: Parameters<typeof stubApi>[0] = {}, id = 'cred1') {
  const sent = stubApi({ 'GET /api/v1/credentials': { status: 200, body: list }, ...more })
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <CredentialDetail id={id} />)
  await screen.findByRole('region', { name: 'The last check' })
  return sent
}

const dialog = () => {
  const d = document.querySelector<HTMLElement>('dialog[open]')
  if (!d) throw new Error('no dialog is open')
  return d
}

describe('the deep check', () => {
  test('asks in the words of the command line, and says above 12 zones that it may not finish', async () => {
    await open([wide, spare])
    fireEvent.click(screen.getByRole('button', { name: 'Check write access' }))
    expect(within(dialog()).getByText(deepQuestion)).toBeTruthy()
    expect(within(dialog()).getByText(/^A deep check of 13 zones/).textContent).toBe(
      'A deep check of 13 zones makes about 42 calls to Cloudflare and may not finish within the minute the daemon waits; a token scoped to fewer zones checks faster.',
    )
  })

  test('the limit counts the zones and accounts of the last report', () => {
    expect(deepLimitText({ zones: zones(12), accounts: [{ id: 'acc1', name: 'Main' }] })).toBe('')
    expect(deepLimitText({ zones: zones(50), accounts: [{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }] })).toMatch(/^A deep check of 50 zones makes about 156 calls/)
    expect(deepLimitText(undefined)).toBe('')
  })

  test('twelve zones are asked without the limit', async () => {
    await open([{ ...main, report: { ...report, zones: zones(12) } }])
    fireEvent.click(screen.getByRole('button', { name: 'Check write access' }))
    expect(dialog().textContent).not.toContain('may not finish')
  })

  test('runs with an indicator of the time, sends deep, and shows the new checklist', async () => {
    let answer: (r: Reply) => void = () => {}
    const sent = await open([main], {
      'POST /api/v1/credentials/cred1/check': () =>
        new Promise<Reply>((resolve) => {
          answer = resolve
        }),
    })
    fireEvent.click(screen.getByRole('button', { name: 'Check write access' }))
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Check write access' }))
    const busy = await screen.findByText('Checking write access')
    expect(busy.getAttribute('role')).toBe('status')
    expect(busy.parentElement?.querySelector('.busy-time')?.textContent).toBe('0 s')
    expect(screen.queryByRole('progressbar')).toBeNull()
    expect(screen.getByRole('button', { name: 'Check' }).hasAttribute('disabled')).toBe(true)
    expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/credentials/cred1/check', body: { deep: true } }])
    await act(async () => answer({ status: 200, body: { ...main, report: { ...report, usable: false } } }))
    await waitFor(() => expect(screen.queryByText('Checking write access')).toBeNull())
    expect(screen.getByText(/Usable:/).parentElement?.textContent).toBe('Usable: no')
  })
})

test('a check only reads, and asks nothing first', async () => {
  const sent = await open([main], { 'POST /api/v1/credentials/cred1/check': { status: 200, body: { ...main, report: { ...report, deep: false } } } })
  fireEvent.click(screen.getByRole('button', { name: 'Check' }))
  await screen.findByText('Write access was not tried.')
  expect(document.querySelector('dialog[open]')).toBeNull()
  expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/credentials/cred1/check', body: { deep: false } }])
})

test('remove needs the label typed, and shows the daemon’s refusal as it is', async () => {
  const why = 'credential cred1 still manages tunnel pco-abc123 in account acc1, record www.example.com'
  const sent = await open([main], { 'DELETE /api/v1/credentials/cred1': { status: 409, body: { error: why, code: 'refused' } } })
  fireEvent.click(screen.getByRole('button', { name: 'Remove' }))
  const remove = within(dialog()).getByRole('button', { name: 'Remove' })
  expect(remove.hasAttribute('disabled')).toBe(true)
  fireEvent.change(within(dialog()).getByRole('textbox'), { target: { value: 'mai' } })
  expect(remove.hasAttribute('disabled')).toBe(true)
  fireEvent.change(within(dialog()).getByRole('textbox'), { target: { value: 'main' } })
  expect(remove.hasAttribute('disabled')).toBe(false)
  fireEvent.click(remove)
  expect((await within(dialog()).findByRole('alert')).textContent).toContain(why)
  expect(writes(sent)).toEqual([{ method: 'DELETE', path: '/api/v1/credentials/cred1', body: undefined }])
})

test('a credential never checked says so', async () => {
  await open([main, spare], {}, 'cred2')
  expect(screen.getByText('This token has no check on record: run a check to see what it can do.')).toBeTruthy()
  expect(screen.getByText('unknown')).toBeTruthy()
})

test('the list: the state in the words of the command line, the depth, the zones left out', async () => {
  stubApi({ 'GET /api/v1/credentials': { status: 200, body: [main, spare] } })
  const { store } = await fakeStore({ state: populated })
  renderPage(store, <Credentials />)
  const table = await screen.findByRole('table', { name: 'Credentials' })
  const cells = (label: string) => {
    const row = within(table)
      .getAllByRole('row')
      .find((r) => r.querySelector('td')?.textContent === label)
    return Object.fromEntries([...(row?.querySelectorAll('td') ?? [])].map((td) => [td.dataset.label, td.textContent]))
  }
  expect(cells('main')).toMatchObject({ State: 'usable', Depth: 'write access proven', Accounts: 'Main', 'Zones left out': 'example.org left out: no DNS read' })
  expect(cells('spare')).toMatchObject({ State: 'unknown', Depth: 'never checked', 'Last check': '-' })
  expect(screen.queryByRole('form', { name: 'Add a Cloudflare API token' })).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: 'Add credential' }))
  expect(screen.getByRole('form', { name: 'Add a Cloudflare API token' })).toBeTruthy()
})

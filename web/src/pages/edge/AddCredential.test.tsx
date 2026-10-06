import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import type { CredentialView, State } from '../../api/types.gen'
import populated from '../../fixtures/populated.json'
import { fakeStore } from '../../test/store'
import { renderPage, stubApi, writes } from '../testing'
import { AddCredential, badTokenText } from './AddCredential'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

const stored = (populated as unknown as State).credentials[0] as CredentialView
const shallow: CredentialView = { ...stored, id: 'c9d0e1f2', label: 'edge', report: stored.report && { ...stored.report, deep: false } }
const token = 'Ab3_dEf-0123456789ghijKLMN'

async function open(answers: Parameters<typeof stubApi>[0]) {
  const sent = stubApi(answers)
  const { store } = await fakeStore({ state: populated })
  const added = vi.fn<(v: CredentialView) => void>()
  renderPage(store, <AddCredential onAdded={added} />)
  return { sent, added }
}

const tokenField = () => screen.getByLabelText('Cloudflare API token') as HTMLInputElement

function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText('Label'), { target: { value: label } })
  fireEvent.change(tokenField(), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: 'Check and add' }))
}

test('the label and the token are sent; the answer is the checklist, write access was not tried, and the token is gone', async () => {
  const { sent, added } = await open({ 'POST /api/v1/credentials': { status: 201, body: shallow } })
  fill(' edge ', ` ${token}\n`)
  await screen.findByText('Write access was not tried.')
  expect(writes(sent)).toEqual([{ method: 'POST', path: '/api/v1/credentials', body: { label: 'edge', token } }])
  expect(added).toHaveBeenCalledWith(shallow)
  expect(screen.getByText('Added credential c9d0e1f2 (edge).')).toBeTruthy()
  expect(screen.getByRole('region', { name: 'Zone example.com' })).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Check write access' })).toBeTruthy()
  expect(tokenField().value).toBe('')
})

test('a refused token shows what it can do and why, is not stored, and is gone from the field', async () => {
  const refused = { label: 'edge', kind: 'scoped', checked: true, id: '', report: { ...stored.report, usable: false, deep: false } }
  const { added } = await open({
    'POST /api/v1/credentials': {
      status: 400,
      body: { error: 'the token cannot be used: dns.read on example.com: grant Zone > DNS > Read on example.com', code: 'invalid', credential: refused },
    },
  })
  fill('edge', token)
  const alert = await screen.findByRole('alert')
  expect(alert.textContent).toContain('the token cannot be used: dns.read on example.com')
  expect(screen.getByText(/Usable:/).parentElement?.textContent).toBe('Usable: no')
  expect(screen.queryByText('Write access was not tried.')).toBeNull()
  expect(added).not.toHaveBeenCalled()
  expect(tokenField().value).toBe('')
})

test('a token of another shape is not sent', async () => {
  const { sent } = await open({})
  fill('edge', 'user@pam!pco=0123')
  expect(screen.getByText(badTokenText)).toBeTruthy()
  expect(sent).toEqual([])
  fill('', token)
  expect(screen.getByText('Give the credential a label.')).toBeTruthy()
  expect(sent).toEqual([])
})

test('check write access asks first, then checks deep and shows the new checklist', async () => {
  const deep: CredentialView = { ...shallow, report: shallow.report && { ...shallow.report, deep: true } }
  const { sent } = await open({ 'POST /api/v1/credentials': { status: 201, body: shallow }, 'POST /api/v1/credentials/c9d0e1f2/check': { status: 200, body: deep } })
  fill('edge', token)
  fireEvent.click(await screen.findByRole('button', { name: 'Check write access' }))
  const dialog = document.querySelector<HTMLElement>('dialog[open]')
  if (!dialog) throw new Error('no dialog')
  expect(dialog.textContent).toContain('A deep check creates and deletes a test DNS record and a test tunnel. Continue?')
  fireEvent.click(within(dialog).getByRole('button', { name: 'Check write access' }))
  await waitFor(() => expect(screen.queryByText('Write access was not tried.')).toBeNull())
  expect(writes(sent).at(-1)).toEqual({ method: 'POST', path: '/api/v1/credentials/c9d0e1f2/check', body: { deep: true } })
})

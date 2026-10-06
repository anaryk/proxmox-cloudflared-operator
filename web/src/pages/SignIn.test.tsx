import { fireEvent, render, screen } from '@testing-library/react'
import { expect, test, vi } from 'vitest'

import { ApiError } from '../api/client'
import { AppStore, StoreProvider } from '../api/store'
import session from '../fixtures/session.json'
import unauthenticated from '../fixtures/unauthenticated.json'
import { flush, memoryStorage } from '../test/store'
import { SignIn } from './SignIn'

function storeWith(reply: (method: string, path: string, body: unknown) => unknown, signedOut = false) {
  const sessionStorage = memoryStorage()
  if (signedOut) sessionStorage.setItem('pco.signedOut', '1')
  return new AppStore({
    storages: { session: sessionStorage, local: memoryStorage() },
    request: async (method, path, body) => {
      const got = reply(method, path, body)
      if (got instanceof ApiError) throw got
      return { status: 200, body: got } as never
    },
    share: () => ({ close: () => {}, resume: () => {}, leading: () => true }),
  })
}

test('after a sign-out: still signed in to Proxmox VE, and a button to sign in with it', async () => {
  const calls: string[] = []
  const store = storeWith((method, path) => {
    calls.push(`${method} ${path}`)
    return path === '/api/session' ? new ApiError(401, unauthenticated) : session
  }, true)
  await store.loadSession()
  const signedIn = vi.fn()
  render(
    <StoreProvider store={store}>
      <SignIn onSignedIn={signedIn} />
    </StoreProvider>,
  )
  expect(screen.getByText('You are signed out of pco. You are still signed in to Proxmox VE in this browser.')).toBeTruthy()
  expect(calls).toEqual(['GET /api/session'])
  fireEvent.click(screen.getByRole('button', { name: 'Sign in with the Proxmox VE session' }))
  await flush()
  expect(calls).toEqual(['GET /api/session', 'POST /api/session/ticket'])
  expect(signedIn).toHaveBeenCalled()
})

test('a ticket Proxmox VE refused as the page loaded is said on the page', async () => {
  const refused = 'Proxmox VE did not accept the session of this browser; sign in to Proxmox VE again'
  const store = storeWith((_m, path) => (path === '/api/session' ? new ApiError(401, unauthenticated) : new ApiError(401, { error: refused, code: 'ticket_invalid' })))
  await store.loadSession()
  expect(store.get().auth).toBe('signed-out')
  render(
    <StoreProvider store={store}>
      <SignIn onSignedIn={() => {}} />
    </StoreProvider>,
  )
  expect(screen.getByRole('alert').textContent).toBe(refused)
})

test('a token: hidden by default, its refusal at the field', async () => {
  const store = storeWith((_m, path) =>
    path === '/api/session/token' ? new ApiError(403, { error: 'pco needs Sys.Audit on /', code: 'forbidden', missing: 'Sys.Audit' }) : new ApiError(401, { ...unauthenticated, ticket: false }),
  )
  await store.loadSession()
  render(
    <StoreProvider store={store}>
      <SignIn onSignedIn={() => {}} />
    </StoreProvider>,
  )
  expect(screen.getByRole('link', { name: 'Proxmox VE' }).getAttribute('href')).toBe(`https://${window.location.hostname}:8006/`)
  const field = screen.getByLabelText('API token') as HTMLInputElement
  expect(field.type).toBe('password')
  expect(field.autocomplete).toBe('off')
  fireEvent.click(screen.getByRole('button', { name: 'Show' }))
  expect(field.type).toBe('text')
  expect(document.querySelector('pre code')?.textContent).toContain('pveum acl modify / --tokens')
  fireEvent.change(field, { target: { value: 'alice@pve!pco=00000000-0000-0000-0000-000000000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))
  expect(await screen.findByText('This user has no Sys.Audit on /: pco cannot show it anything.')).toBeTruthy()
  expect(field.getAttribute('aria-invalid')).toBe('true')
  expect(screen.getByText(/not affiliated with Proxmox Server Solutions GmbH or Cloudflare, Inc\./)).toBeTruthy()
})

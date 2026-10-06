import { fireEvent, render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { ApiError } from '../api/client'
import { StoreProvider } from '../api/store'
import populated from '../fixtures/populated.json'
import session from '../fixtures/session.json'
import unauthenticated from '../fixtures/unauthenticated.json'
import { fakeStore, flush } from '../test/store'
import { SignInDialog } from './SignInDialog'

test('signed in to Proxmox VE in another tab, the user goes on over the page without a reload', async () => {
  let signedInElsewhere = false
  const { store } = await fakeStore({
    state: populated,
    answers: {
      'GET /api/v1/state': (c) => (c.opts?.ifNoneMatch === populated.digest ? { status: 304, body: undefined } : { status: 200, body: populated, etag: populated.digest }),
      'POST /api/session/ticket': () =>
        signedInElsewhere
          ? { status: 200, body: session }
          : new ApiError(401, { error: 'this browser has no Proxmox VE session for this name', code: 'ticket_invalid' }),
    },
  })
  const page = store.get().state
  render(
    <StoreProvider store={store}>
      <SignInDialog />
    </StoreProvider>,
  )
  // the session idled out; the browser has no ticket of Proxmox VE now
  store.clientHooks().unauthenticated({ ...unauthenticated, ticket: false })
  await flush()
  expect(screen.getByRole('heading', { name: 'Your session has ended' })).toBeTruthy()
  expect(screen.getByRole('link', { name: 'Proxmox VE' }).getAttribute('target')).toBe('_blank')
  expect(screen.getByText(/then come back and sign in here with its session; this page keeps what it shows/)).toBeTruthy()

  signedInElsewhere = true
  fireEvent.click(screen.getByRole('button', { name: 'Sign in with the Proxmox VE session' }))
  await flush()
  await flush()
  expect(store.get().signInNeeded).toBe(false)
  expect(store.get().state).toBe(page)
  expect(screen.queryByRole('heading', { name: 'Your session has ended' })).toBeNull()
})

test('with the Proxmox VE session still in the browser, the dialog does not even stay open', async () => {
  const { store, calls } = await fakeStore({ state: populated, answers: { 'POST /api/session/ticket': () => ({ status: 200, body: session }) } })
  store.clientHooks().unauthenticated(unauthenticated)
  await flush()
  expect(calls.map((c) => `${c.method} ${c.path}`)).toContain('POST /api/session/ticket')
  expect(store.get().signInNeeded).toBe(false)
})

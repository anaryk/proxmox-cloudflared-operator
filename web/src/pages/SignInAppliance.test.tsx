import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { expect, test, vi } from 'vitest'

import { ApiError } from '../api/client'
import type { ErrorFields } from '../api/errors'
import { AppStore, StoreProvider } from '../api/store'
import session from '../fixtures/session.json'
import { memoryStorage } from '../test/store'
import { SignIn } from './SignIn'

const appliance = { code: 'unauthenticated', methods: ['password', 'token'], ticket: false, realms: ['pve', 'pam', 'ad'] }
const signedIn = { ...session, method: 'password', profile: 'appliance' }
// A loaded machine may take its time to render what the answer changed: every
// wait is for the page to show it, never for a moment to pass.
const slow = { timeout: 10_000 }

interface Call {
  method: string
  path: string
  body: unknown
}

// page renders the sign-in page of the appliance, whose web process answers
// with reply.
async function page(reply: (c: Call) => unknown, unauth: ErrorFields = appliance) {
  const calls: Call[] = []
  const store = new AppStore({
    storages: { session: memoryStorage(), local: memoryStorage() },
    request: async (method, path, body) => {
      const c = { method, path, body }
      calls.push(c)
      const got = path === '/api/session' && method === 'GET' ? new ApiError(401, unauth) : reply(c)
      if (got instanceof ApiError) throw got
      return { status: 200, body: got } as never
    },
    share: () => ({ close: () => {}, resume: () => {}, leading: () => true }),
  })
  await store.loadSession()
  const done = vi.fn()
  render(
    <StoreProvider store={store}>
      <SignIn onSignedIn={done} />
    </StoreProvider>,
  )
  return { calls, done, store }
}

const field = (label: string) => screen.getByLabelText(label) as HTMLInputElement

function signInWith(user: string, password: string) {
  fireEvent.change(field('User'), { target: { value: user } })
  fireEvent.change(field('Password'), { target: { value: password } })
  fireEvent.click(within(screen.getByRole('form', { name: 'With your Proxmox VE user' })).getByRole('button', { name: 'Sign in' }))
}

test('the user, a realm of the node and the password; nothing of the password stays', async () => {
  const { calls, done, store } = await page(() => signedIn)
  expect(screen.queryByRole('button', { name: 'Sign in with the Proxmox VE session' })).toBeNull()
  const realm = screen.getByLabelText('Realm') as HTMLSelectElement
  expect([...realm.options].map((o) => o.value)).toEqual(['pve', 'pam', 'ad'])
  expect(realm.value).toBe('pve')
  fireEvent.change(realm, { target: { value: 'pam' } })
  expect(field('Password').type).toBe('password')
  expect(field('Password').autocomplete).toBe('current-password')
  expect(field('User').autocomplete).toBe('username')

  signInWith(' alice ', 'pw-Zebra-7731')
  await waitFor(() => expect(done).toHaveBeenCalled(), slow)

  expect(calls.slice(1)).toEqual([{ method: 'POST', path: '/api/session/password', body: { user: 'alice', realm: 'pam', password: 'pw-Zebra-7731' } }])
  expect(JSON.stringify(store.get())).not.toContain('pw-Zebra-7731')
})

test('a wrong password is said, and the password field is empty again', async () => {
  await page(() => new ApiError(401, { code: 'ticket_invalid', error: 'Proxmox VE did not accept the user and password' }))
  signInWith('alice', 'wrong')
  expect((await screen.findByRole('alert', {}, slow)).textContent).toBe('Proxmox VE did not accept the user and password')
  expect(field('Password').value).toBe('')
  expect(field('User').value).toBe('alice')
})

test('the refusals of root, of a locked account and of a user without Sys.Audit', async () => {
  const answers = [
    new ApiError(403, { code: 'forbidden', error: 'Sign in with a user that has Sys.Modify on /, or with an API token' }),
    new ApiError(429, { code: 'rate_limited', error: 'too many failed sign-ins of this account; try again in 60 s, or sign in with an API token', retryAfter: 60 }),
    new ApiError(403, { code: 'forbidden', error: 'pco needs Sys.Audit on /', missing: 'Sys.Audit' }),
  ]
  await page(() => answers.shift())
  for (const want of [
    'Sign in with a user that has Sys.Modify on /, or with an API token',
    'Too many failed sign-ins of this account; try again in 60 s, or sign in with an API token',
    'This user has no Sys.Audit on /: pco cannot show it anything.',
  ]) {
    signInWith('root', 'x')
    await waitFor(() => expect(screen.getByRole('alert').textContent).toBe(want), slow)
  }
})

test('the second factor: a wrong code, then the right one', async () => {
  const codes: unknown[] = []
  const { done } = await page((c) => {
    if (c.path === '/api/session/password') return new ApiError(401, { code: 'second_factor', error: 'Proxmox VE asks for the second factor of this account', kinds: ['recovery', 'totp'] })
    codes.push(c.body)
    return codes.length === 1 ? new ApiError(401, { code: 'second_factor', error: 'Proxmox VE did not accept the code', kinds: ['recovery', 'totp'] }) : signedIn
  })
  signInWith('fred', 'pw')
  await screen.findByLabelText('Code', {}, slow)

  expect(screen.queryByLabelText('Password')).toBeNull()
  expect(screen.getByText('The second factor of fred')).toBeTruthy()
  expect(screen.getByText('The code of your authenticator app, or one of your recovery codes.')).toBeTruthy()
  expect(field('Code').autocomplete).toBe('one-time-code')
  expect(screen.queryByRole('alert')).toBeNull()

  fireEvent.change(field('Code'), { target: { value: '000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Verify' }))
  expect(await screen.findByText('Proxmox VE did not accept the code', {}, slow)).toBeTruthy()
  expect(field('Code').getAttribute('aria-invalid')).toBe('true')

  fireEvent.change(field('Code'), { target: { value: ' 424242 ' } })
  fireEvent.click(screen.getByRole('button', { name: 'Verify' }))
  await waitFor(() => expect(done).toHaveBeenCalled(), slow)
  expect(codes).toEqual([{ code: '000000' }, { code: '424242' }])
})

test('a step that is over goes back to the password', async () => {
  await page((c) =>
    c.path === '/api/session/password'
      ? new ApiError(401, { code: 'second_factor', error: 'asks', kinds: ['totp'] })
      : new ApiError(401, { code: 'ticket_invalid', error: 'Proxmox VE did not accept the code three times. Start again with the password' }),
  )
  signInWith('fred', 'pw')
  expect(await screen.findByText('The code of your authenticator app.', {}, slow)).toBeTruthy()
  expect(field('Code').inputMode).toBe('numeric')
  fireEvent.change(field('Code'), { target: { value: '000000' } })
  fireEvent.click(screen.getByRole('button', { name: 'Verify' }))
  await waitFor(() => expect(screen.getByRole('alert').textContent).toBe('Proxmox VE did not accept the code three times. Start again with the password'), slow)
  expect(field('Password').value).toBe('')
  expect(field('User').value).toBe('fred')

  // and the link does the same at any time
  signInWith('fred', 'pw')
  fireEvent.click(await screen.findByRole('button', { name: 'Start again with the password' }, slow))
  expect(field('Password')).toBeTruthy()
})

test('a security key cannot work here, and the page says so', async () => {
  const sentence = "This account's second factor is a security key, which works only on Proxmox VE's own page. Use a TOTP or recovery code, or an API token."
  await page(() => new ApiError(401, { code: 'second_factor_key', error: sentence, kinds: ['webauthn'] }))
  signInWith('gina', 'pw')
  expect((await screen.findByRole('alert', {}, slow)).textContent).toBe(sentence)
  expect(screen.queryByLabelText('Code')).toBeNull()
})

test('the token goes past the password', async () => {
  const { calls, done } = await page(() => signedIn)
  expect(screen.getByRole('heading', { name: 'Or with an API token' })).toBeTruthy()
  fireEvent.change(field('API token'), { target: { value: 'alice@pve!pco=00000000-0000-0000-0000-000000000000' } })
  fireEvent.click(within(screen.getByRole('form', { name: 'Or with an API token' })).getByRole('button', { name: 'Sign in' }))
  await waitFor(() => expect(done).toHaveBeenCalled(), slow)
  expect(calls.at(-1)?.path).toBe('/api/session/token')
})

test('without the realms of the node, the realm is typed', async () => {
  const { calls } = await page(() => signedIn, { code: 'unauthenticated', methods: ['password', 'token'], ticket: false })
  expect(screen.getByText('The realm of the user, such as pam or pve: the list of the node could not be read.')).toBeTruthy()
  fireEvent.change(field('Realm'), { target: { value: 'pve' } })
  signInWith('alice', 'pw')
  await waitFor(() => expect(calls.at(-1)?.body).toEqual({ user: 'alice', realm: 'pve', password: 'pw' }), slow)
})

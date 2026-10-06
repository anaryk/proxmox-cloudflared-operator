import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { ApiError } from '../api/client'
import { AppStore } from '../api/store'
import unauthenticated from '../fixtures/unauthenticated.json'
import populated from '../fixtures/populated.json'
import { fakeStore, flush, memoryStorage } from '../test/store'
import { App, loadPart, nextOf } from './App'
import { navigate } from './router'

test.each([
  ['?next=%2Froutes%3Fowner%3Dqemu%2F101', '/routes?owner=qemu/101'],
  ['?next=%2F%2Fevil.example', '/'],
  ['?next=https%3A%2F%2Fevil.example', '/'],
  ['?next=%2F%5Cevil.example', '/'],
  ['?next=%2Fsignin', '/'],
  ['', '/'],
])('after the sign-in, %s goes to %s', (search, to) => {
  expect(nextOf(search)).toBe(to)
})

test('a part that does not load raises the reload banner', async () => {
  const store = new AppStore({ storages: { session: memoryStorage(), local: memoryStorage() } })
  await expect(loadPart(() => Promise.reject(new TypeError('Failed to fetch dynamically imported module')), store)).rejects.toThrow()
  expect(store.get().skew).toBe(true)
})

test('signed out, the page goes to the sign-in and remembers where the user was going', async () => {
  navigate('/events?level=error')
  const store = new AppStore({
    storages: { session: memoryStorage(), local: memoryStorage() },
    request: async () => {
      throw new ApiError(401, { ...unauthenticated, ticket: false })
    },
    share: () => ({ close: () => {}, resume: () => {}, leading: () => true }),
  })
  render(<App store={store} />)
  await flush()
  await flush()
  expect(window.location.pathname).toBe('/signin')
  expect(new URLSearchParams(window.location.search).get('next')).toBe('/events?level=error')
  expect(screen.getByRole('heading', { name: 'Sign in to pco' })).toBeTruthy()
  store.stop()
})

test('signed in: the shell, with its skip link, banners and the strip', async () => {
  navigate('/')
  const { store } = await fakeStore({ state: populated })
  render(<App store={store} />)
  expect(screen.getByRole('link', { name: 'Skip to content' }).getAttribute('href')).toBe('#main')
  expect(screen.getByRole('main').id).toBe('main')
  expect(screen.getByRole('heading', { name: '1 problem' })).toBeTruthy()
  expect(document.querySelector('[data-banner="egress"]')).toBeTruthy()
  expect(document.querySelector('details.strip')).toBeTruthy()
  store.stop()
})

import { render, screen } from '@testing-library/react'
import { lazy, Suspense } from 'react'
import { expect, test, vi } from 'vitest'

import { AppStore } from '../api/store'
import { memoryStorage } from '../test/store'
import { loadPart, Part } from './part'

const notLoaded = () => Promise.reject(new TypeError('Failed to fetch dynamically imported module'))

test('a part that does not load raises the reload banner', async () => {
  const store = new AppStore({ storages: { session: memoryStorage(), local: memoryStorage() } })
  await expect(loadPart(notLoaded, store)).rejects.toThrow()
  expect(store.get().skew).toBe(true)
})

test('a part that does not load says so in its place, and the rest of the page stays', async () => {
  const store = new AppStore({ storages: { session: memoryStorage(), local: memoryStorage() } })
  const Missing = lazy(() => loadPart(notLoaded, store))
  // React reports the error the part caught.
  const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined)
  render(
    <>
      <p>the rest of the page</p>
      <Part what="The map">
        <Suspense fallback={<p>loading</p>}>
          <Missing />
        </Suspense>
      </Part>
    </>,
  )
  expect(await screen.findByText('The map did not load')).toBeTruthy()
  expect(screen.getByText('the rest of the page')).toBeTruthy()
  expect(store.get().skew).toBe(true)
  logged.mockRestore()
})

test('a part that loads is drawn as it is', async () => {
  const Here = lazy(() => loadPart(() => Promise.resolve({ default: () => <p>the part</p> })))
  render(
    <Part what="The map">
      <Suspense fallback={<p>loading</p>}>
        <Here />
      </Suspense>
    </Part>,
  )
  expect(await screen.findByText('the part')).toBeTruthy()
})

test('a part whose code fails as it draws says so, not that pco was updated', () => {
  const Broken = () => {
    throw new Error('a bug of the part')
  }
  const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined)
  render(
    <Part what="The map">
      <Broken />
    </Part>,
  )
  expect(screen.getByText('The map could not be drawn')).toBeTruthy()
  expect(screen.queryByText(/pco was updated/)).toBeNull()
  logged.mockRestore()
})

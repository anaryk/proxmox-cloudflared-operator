import { render, screen } from '@testing-library/react'
import { expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import { ToastProvider } from '../components/Toast'
import untagged from '../fixtures/untagged.json'
import { fakeStore } from '../test/store'
import { Page } from './Page'
import { match } from './router'

// The chunk of the networks page is gone, as after an update of pco.
vi.mock('../pages/networks/NetworksPage', () => {
  throw new Error('the chunk of the networks page is gone')
})

test('a page that did not load is not taken for the next one', async () => {
  const { store } = await fakeStore({ state: untagged })
  // React reports what the part caught.
  const logged = vi.spyOn(console, 'error').mockImplementation(() => undefined)
  const page = (path: string) => (
    <StoreProvider store={store}>
      <ToastProvider>
        <Page view={match(path)} />
      </ToastProvider>
    </StoreProvider>
  )
  const { rerender } = render(page('/networks'))
  expect(await screen.findByText('This page did not load')).toBeTruthy()

  rerender(page('/events'))
  expect(await screen.findByRole('heading', { level: 1, name: 'Events' })).toBeTruthy()
  expect(screen.queryByText('This page did not load')).toBeNull()
  logged.mockRestore()
})

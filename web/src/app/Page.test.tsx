import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import { ToastProvider } from '../components/Toast'
import untagged from '../fixtures/untagged.json'
import { fakeStore } from '../test/store'
import { Page } from './Page'
import { match } from './router'

test('what the address names is shown as untrusted text', () => {
  render(<Page view={match('/routes/evil%E2%80%AEmoc.example.com', '?owner=qemu%2F101%E2%80%8B')} />)
  const title = screen.getByRole('heading', { level: 1 })
  expect(title.textContent).toBe('evil⟨U+202E⟩moc.example.com')
  expect(title.querySelector('bdi')?.getAttribute('dir')).toBe('ltr')
  expect(document.querySelector('.page-description')?.textContent).toBe('The route of qemu/101⟨U+200B⟩.')
})

test('a zone and an account too', () => {
  const { unmount } = render(<Page view={match('/edge/zones/example%E2%80%AE.com')} />)
  expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('example⟨U+202E⟩.com')
  unmount()
  render(<Page view={match('/edge/tunnels/acc%E2%80%AE1')} />)
  expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Tunnel in account acc⟨U+202E⟩1')
})

test.each([
  ['/events', 'Events'],
  ['/doctor', 'Doctor'],
  ['/networks', 'Networks'],
])('%s is the page of %s', async (path, title) => {
  const { store } = await fakeStore({ state: untagged })
  render(
    <StoreProvider store={store}>
      <ToastProvider>
        <Page view={match(path)} />
      </ToastProvider>
    </StoreProvider>,
  )
  expect(screen.getByRole('heading', { level: 1, name: title })).toBeTruthy()
  expect(screen.queryByText(/not part of this build yet/)).toBeNull()
})

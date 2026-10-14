import { render, screen, within } from '@testing-library/react'
import { expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import type { State } from '../api/types.gen'
import { ToastProvider } from '../components/Toast'
import cluster from '../fixtures/cluster.json'
import populated from '../fixtures/populated.json'
import untagged from '../fixtures/untagged.json'
import { fakeStore } from '../test/store'
import { Page } from './Page'
import { match } from './router'

test('what the address names is shown as untrusted text', async () => {
  const { store } = await fakeStore({ state: populated })
  render(
    <StoreProvider store={store}>
      <ToastProvider>
        <Page view={match('/routes/evil%E2%80%AEmoc.example.com', '?owner=qemu%2F101%E2%80%8B')} />
      </ToastProvider>
    </StoreProvider>,
  )
  const title = within(screen.getByRole('complementary')).getByRole('heading', { level: 2 })
  expect(title.textContent).toBe('evil⟨U+202E⟩moc.example.com')
  expect(title.querySelector('bdi')?.getAttribute('dir')).toBe('ltr')
  expect(screen.getByText(/asks for/).textContent).toBe('qemu/101⟨U+200B⟩ asks for evil⟨U+202E⟩moc.example.com no longer.')
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
  expect(await screen.findByRole('heading', { level: 1, name: title })).toBeTruthy()
  expect(screen.queryByText(/not part of this build yet/)).toBeNull()
})

test('the title of a tunnel pinned to a node says so, also when the account has no other', async () => {
  const st = cluster as unknown as State
  const page = async (state: State, path: string, search: string) => {
    const { store } = await fakeStore({ state })
    return render(
      <StoreProvider store={store}>
        <ToastProvider>
          <Page view={match(path, search)} />
        </ToastProvider>
      </StoreProvider>,
    )
  }
  const named = await page(st, '/edge/tunnels/acc1', '?tunnel=pco-abc123-pve2')
  expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Tunnel Main · pinned to pve2')
  named.unmount()

  const shared = await page(st, '/edge/tunnels/acc1', '')
  expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Tunnel in account acc1')
  shared.unmount()

  const onlyPinned: State = { ...st, tunnels: st.tunnels.filter((t) => t.accountId !== 'acc1' || t.kind === 'pinned') }
  await page(onlyPinned, '/edge/tunnels/acc1', '')
  expect(screen.getByRole('heading', { level: 1 }).textContent).toBe('Tunnel Main · pinned to pve2')
})

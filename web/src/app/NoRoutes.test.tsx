import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import type { State } from '../api/types.gen'
import empty from '../fixtures/empty.json'
import taggedEmpty from '../fixtures/tagged-empty.json'
import untagged from '../fixtures/untagged.json'
import { fakeStore } from '../test/store'
import { NoRoutes } from './NoRoutes'

test('the documentation is that of the version that runs', async () => {
  const { store } = await fakeStore({ state: untagged, session: { version: '0.3.0' } })
  render(
    <StoreProvider store={store}>
      <NoRoutes state={untagged as unknown as State} gateTag="cf-tunnel" />
    </StoreProvider>,
  )
  expect(screen.getByRole('link', { name: 'How to write routes in the Notes' }).getAttribute('href')).toBe(
    'https://github.com/anaryk/proxmox-cloudflared-operator/blob/v0.3.0/docs/annotations.md',
  )
})

test('the same standing problem twice is shown twice', () => {
  const st = { ...(empty as unknown as State), problems: ['a problem', 'a problem'] }
  const { container } = render(<NoRoutes state={st} />)
  expect(container.querySelectorAll('li')).toHaveLength(2)
})

test('no guest carries the tag: the tag, the Notes and the documentation', () => {
  const { container } = render(<NoRoutes state={untagged as unknown as State} gateTag="cf-tunnel" />)
  expect(screen.getByText('No guest carries the tag cf-tunnel')).toBeTruthy()
  expect(container.querySelector('code')?.textContent).toBe('```cf-tunnel\napp.example.com -> :3000\n```')
  expect(screen.getByRole('link', { name: 'How to write routes in the Notes' }).getAttribute('href')).toMatch(/docs\/annotations\.md$/)
  // admission tag: nothing about approvals
  expect(container.textContent).not.toContain('approve')
})

test('guests carry the tag but name no hostname, and new guests wait for approval', () => {
  const { container } = render(<NoRoutes state={taggedEmpty as unknown as State} gateTag="cf-tunnel" />)
  expect(screen.getByText('3 guests carry the tag cf-tunnel, but none names a hostname in its Notes')).toBeTruthy()
  expect(screen.getByRole('link', { name: 'The guests' }).getAttribute('href')).toBe('/guests')
  expect(container.textContent).toContain('Admission is set to approve: the routes of a new guest wait until an admin approves the guest.')
})

test('before the first cycle it is neither: waiting, with the standing problems', () => {
  const st = { ...(empty as unknown as State), problems: ['no writer identity; run pco setup'] }
  render(<NoRoutes state={st} gateTag="cf-tunnel" />)
  expect(screen.getByText('Waiting for the first cycle')).toBeTruthy()
  expect(screen.getByText('no writer identity; run pco setup')).toBeTruthy()
  expect(screen.queryByText(/No guest carries/)).toBeNull()
})

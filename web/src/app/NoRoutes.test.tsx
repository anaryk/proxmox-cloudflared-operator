import { render, screen } from '@testing-library/react'
import { expect, test } from 'vitest'

import type { State } from '../api/types.gen'
import empty from '../fixtures/empty.json'
import taggedEmpty from '../fixtures/tagged-empty.json'
import untagged from '../fixtures/untagged.json'
import { NoRoutes } from './NoRoutes'

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

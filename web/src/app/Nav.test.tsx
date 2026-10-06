import { render, screen, within } from '@testing-library/react'
import { expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import type { State } from '../api/types.gen'
import empty from '../fixtures/empty.json'
import populated from '../fixtures/populated.json'
import rogue from '../fixtures/rogue.json'
import { fakeStore } from '../test/store'
import { countersOf, Nav } from './Nav'
import { navigate } from './router'

test('the counters of the populated fixture', () => {
  // routes: the one in conflict; guests: two waiting and the conflict; edge:
  // the credential never checked, the frozen zone and the rogue connector
  expect(countersOf(populated as unknown as State, 'admin')).toEqual({
    routes: 1,
    guests: 3,
    credentials: 1,
    zones: 1,
    tunnels: 1,
    edge: 3,
    doctor: undefined,
  })
})

test('the counters of the rogue fixture: only the two connectors pco does not run', () => {
  expect(countersOf(rogue as unknown as State, 'reader')).toMatchObject({ routes: 0, guests: 0, credentials: 0, zones: 0, tunnels: 2, edge: 2 })
})

test('the doctor counter is the last run of this browser, for admins only', () => {
  const st = empty as unknown as State
  expect(countersOf(st, 'admin').doctor).toBeUndefined()
  expect(countersOf(st, 'admin', { fail: 2 }).doctor).toBe(2)
  expect(countersOf(st, 'reader', { fail: 2 }).doctor).toBeUndefined()
})

test('the groups of the mockup, no setup item, the counters and the current page', async () => {
  const { store } = await fakeStore({ state: populated })
  store.setDoctorLast({ fail: 4, at: '2026-10-01T12:00:00Z' })
  navigate('/routes/plan')
  render(
    <StoreProvider store={store}>
      <Nav open={false} collapsed={false} onCollapse={() => {}} onAbout={() => {}} />
    </StoreProvider>,
  )
  const nav = screen.getByRole('navigation', { name: 'Main' })
  const links = within(nav).getAllByRole('link')
  expect(links.map((l) => l.querySelector('.nav-label')?.textContent)).toEqual([
    'Overview',
    'Routes',
    'Guests',
    'Networks',
    'Credentials',
    'Zones',
    'Tunnels',
    'Events',
    'Doctor',
    'Settings',
  ])
  expect(within(screen.getByRole('group', { name: 'Edge' })).getAllByRole('link')).toHaveLength(3)
  expect(within(screen.getByRole('group', { name: 'Operate' })).getAllByRole('link')).toHaveLength(3)
  const count = (name: string) => links.find((l) => l.querySelector('.nav-label')?.textContent === name)?.querySelector('.count')?.firstChild?.textContent
  expect([count('Routes'), count('Guests'), count('Credentials'), count('Zones'), count('Tunnels'), count('Doctor')]).toEqual(['1', '3', '1', '1', '1', '4'])
  expect(links.find((l) => l.getAttribute('aria-current') === 'page')?.textContent).toBe('Routes1 routes that are not active')
  expect(nav.textContent).not.toContain('setup')
})

import { render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import type { State, TrafficView, TunnelTraffic } from '../api/types.gen'
import first from '../fixtures/first-run.json'
import populated from '../fixtures/populated.json'
import trafficFixture from '../fixtures/traffic.json'
import chains from '../flow/testdata/chains.json'
import traffic from '../flow/testdata/traffic.json'
import { StatTiles } from './StatTiles'

beforeEach(() => {
  vi.stubEnv('TZ', 'UTC')
})

afterEach(() => {
  vi.unstubAllEnvs()
})

const tile = (name: string) => within(screen.getByRole('region', { name }))
const text = (name: string) => screen.getByRole('region', { name }).textContent

test('the routes by state, the active ones first', () => {
  render(<StatTiles state={chains as unknown as State} traffic={traffic as unknown as TrafficView} />)
  expect(tile('Routes').getByText('4').closest('.tile-big')?.textContent).toBe('4 of 11 active')
  expect([...screen.getByRole('region', { name: 'Routes' }).querySelectorAll('.tile-sub li')].map((li) => li.textContent)).toEqual(['unreachable 1', 'withdrawn 1', 'conflict 1', 'no-zone 1', 'held 1', 'rejected 1', 'frozen 1'])
})

test('the requests of every tunnel, their errors and the chart of 15 minutes', () => {
  const tv = trafficFixture as unknown as TrafficView
  const last = tv.tunnels[0]?.samples.at(-1)
  render(<StatTiles state={populated as unknown as State} traffic={tv} />)
  expect(tile('Edge traffic').getByText(last?.rps.toFixed(1) ?? '').closest('.tile-big')?.textContent).toBe(`${last?.rps.toFixed(1)} req/s`)
  expect(text('Edge traffic')).toContain(`${last?.errorsPerSec.toFixed(1)} errors/s`)
  expect(text('Edge traffic')).toContain('3 requests in flight')
  expect(tile('Edge traffic').getByRole('img', { name: 'Requests to the connectors' })).toBeTruthy()
})

test('a stale tunnel is left out of the sum', () => {
  const tv = traffic as unknown as TrafficView
  render(<StatTiles state={chains as unknown as State} traffic={tv} />)
  // the lab tunnel's 4 req/s are stale
  expect(screen.getByRole('region', { name: 'Edge traffic' }).querySelector('.tile-big')?.textContent).toBe(`${tv.tunnels[0]?.samples.at(-1)?.rps.toFixed(1)} req/s`)
})

test('every tunnel stale: no data since its last sample', () => {
  const tv = trafficFixture as unknown as TrafficView
  const stale: TrafficView = { ...tv, tunnels: tv.tunnels.map((t) => ({ ...t, stale: true })) }
  render(<StatTiles state={populated as unknown as State} traffic={stale} />)
  expect(screen.getByRole('region', { name: 'Edge traffic' }).querySelector('.tile-big')?.textContent).toBe('– req/s')
  expect(text('Edge traffic')).toContain('no data since 2026-10-01 12:00:05 +00:00')
})

test('the connectors: ready, connections, where they land, and those pco does not run', () => {
  render(<StatTiles state={populated as unknown as State} traffic={trafficFixture as unknown as TrafficView} />)
  expect(screen.getByRole('region', { name: 'Connectors' }).querySelector('.tile-big')?.textContent).toBe('1 of 1 ready')
  expect(text('Connectors')).toContain('4 connections')
  expect(text('Connectors')).toContain('fra08 · prg01')
  expect(tile('Connectors').getByRole('link', { name: '1 connector not run by pco' }).getAttribute('href')).toBe('/edge/tunnels')
})

test("an edge location is Cloudflare's text, shown as such", () => {
  const tv = trafficFixture as unknown as TrafficView
  const odd = { ...tv, tunnels: tv.tunnels.map((t) => ({ ...t, edges: [{ ...(t.edges[0] as TunnelTraffic['edges'][number]), location: 'fra\u202e08' }] })) }
  render(<StatTiles state={populated as unknown as State} traffic={odd} />)
  const where = screen.getByRole('region', { name: 'Connectors' }).querySelector('.tile-sub .mono') as HTMLElement
  expect(where.textContent).toBe('fra\u27e8U+202E\u27e908')
  expect(where.querySelector('bdi')).toBeTruthy()
})

test('what needs a person, each where it is dealt with', () => {
  render(<StatTiles state={populated as unknown as State} />)
  expect(screen.getByRole('region', { name: 'Needs you' }).querySelector('.tile-big')?.textContent).toBe('9 items')
  expect(screen.getByRole('region', { name: 'Needs you' }).querySelector('.tile-sub li')?.textContent).toBe('1 problem')
  expect(
    tile('Needs you')
      .getAllByRole('link')
      .map((a) => [a.textContent, a.getAttribute('href')]),
  ).toEqual([
    ['4 confirmations waiting', '/routes/plan'],
    ['2 guests waiting for approval', '/guests'],
    ['1 DNS record in the way', '/routes/plan'],
    ['1 lost name', '/routes/plan'],
  ])
})

test('no traffic yet, and nothing needing a person', () => {
  const st = { ...(populated as unknown as State), problems: [], waiting: [], unapproved: [], conflicts: [], lost: [] }
  render(<StatTiles state={st} />)
  expect(text('Edge traffic')).toContain('No figures from the connectors yet.')
  expect(text('Needs you')).toContain('Nothing needs you.')
})

test('before the first cycle the tiles wait for it', () => {
  render(<StatTiles state={first as unknown as State} />)
  for (const name of ['Routes', 'Edge traffic', 'Connectors']) expect(text(name)).toContain('waiting for the first cycle')
  expect(text('Needs you')).toContain('1 problem')
})

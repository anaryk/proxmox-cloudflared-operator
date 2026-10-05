import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { partsIn, Time } from './Time'

// The browser runs in Prague, the node in New York.
beforeEach(() => {
  vi.stubEnv('TZ', 'Europe/Prague')
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-10-05T15:00:00Z'))
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllEnvs()
})

describe('Time', () => {
  test('today: the local time and its offset', () => {
    const { container } = render(<Time at="2026-10-05T10:01:05Z" nodeZone="America/New_York" />)
    const time = container.querySelector('time')
    expect(time?.textContent).toBe('12:01:05 +02:00')
    expect(time?.getAttribute('datetime')).toBe('2026-10-05T10:01:05Z')
  })

  test('another day: the date as well', () => {
    const { container } = render(<Time at="2026-10-04T21:59:59Z" />)
    expect(container.querySelector('time')?.textContent).toBe('2026-10-04 23:59:59 +02:00')
  })

  test('the day is the local one', () => {
    // 22:30 UTC on the 4th is half past midnight of the 5th in Prague.
    const { container } = render(<Time at="2026-10-04T22:30:00Z" />)
    expect(container.querySelector('time')?.textContent).toBe('00:30:00 +02:00')
  })

  test('the tooltip: the node time and UTC', () => {
    render(<Time at="2026-10-05T10:01:05Z" nodeZone="America/New_York" />)
    const tip = screen.getByRole('tooltip', { hidden: true })
    expect(tip.textContent).toBe('Node (America/New_York): 2026-10-05 06:01:05 -04:00UTC: 2026-10-05 10:01:05')
    expect(tip.hidden).toBe(true)
    const time = screen.getByText('12:01:05 +02:00')
    expect(time.getAttribute('aria-describedby')).toBe(tip.id)
    fireEvent.mouseEnter(time)
    expect(tip.hidden).toBe(false)
  })

  test('without a zone of the node, or with one the browser does not know, UTC only', () => {
    const { rerender } = render(<Time at="2026-10-05T10:01:05Z" />)
    expect(screen.getByRole('tooltip', { hidden: true }).textContent).toBe('UTC: 2026-10-05 10:01:05')
    rerender(<Time at="2026-10-05T10:01:05Z" nodeZone="Mars/Olympus" />)
    expect(screen.getByRole('tooltip', { hidden: true }).textContent).toBe('UTC: 2026-10-05 10:01:05')
  })

  test.each(['0001-01-01T00:00:00Z', '', 'yesterday'])('a time never set or not a time: %j', (at) => {
    const { container } = render(<Time at={at} />)
    expect(container.textContent).toBe('-')
    expect(container.querySelector('time')).toBeNull()
  })
})

test('partsIn writes the offset of UTC as +00:00', () => {
  expect(partsIn(new Date('2026-01-01T00:00:00Z'), 'UTC')).toEqual({ date: '2026-01-01', time: '00:00:00', offset: '+00:00' })
})

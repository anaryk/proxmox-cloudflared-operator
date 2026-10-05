import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { Busy, elapsedText } from './Busy'

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-05T10:00:00Z'))
})

afterEach(() => {
  vi.useRealTimers()
})

test('the time since it began, counted on', () => {
  const { container } = render(<Busy label="Checking the token" />)
  expect(screen.getByRole('status').textContent).toBe('Checking the token')
  expect(container.querySelector('.busy-time')?.textContent).toBe('0 s')
  act(() => {
    vi.advanceTimersByTime(12_000)
  })
  expect(container.querySelector('.busy-time')?.textContent).toBe('12 s')
  act(() => {
    vi.advanceTimersByTime(65_000)
  })
  expect(container.querySelector('.busy-time')?.textContent).toBe('1 min 17 s')
})

test('from a start before it was shown', () => {
  const { container } = render(<Busy label="Diagnosing" since={Date.now() - 30_000} />)
  expect(container.querySelector('.busy-time')?.textContent).toBe('30 s')
})

test('never a percentage', () => {
  const { container } = render(<Busy label="Diagnosing" />)
  act(() => {
    vi.advanceTimersByTime(90_000)
  })
  expect(container.textContent).not.toMatch(/%/)
  expect(container.querySelector('progress, [role="progressbar"]')).toBeNull()
})

test.each([
  [0, '0 s'],
  [999, '0 s'],
  [59_999, '59 s'],
  [60_000, '1 min 0 s'],
  [-5, '0 s'],
])('elapsedText(%d)', (ms, want) => {
  expect(elapsedText(ms)).toBe(want)
})

import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import { StoreProvider } from '../api/store'
import untagged from '../fixtures/untagged.json'
import { fakeStore } from '../test/store'
import { Activity, followInput } from './Activity'

afterEach(() => {
  vi.useRealTimers()
})

test('input touches the session at most once a minute', () => {
  let now = 0
  const touch = vi.fn()
  const target = new EventTarget()
  const stop = followInput(target, touch, () => now)
  // a key, a click or the wheel every second for three minutes
  for (; now < 180_000; now += 1000) {
    target.dispatchEvent(new Event(['keydown', 'pointerdown', 'wheel'][(now / 1000) % 3] ?? 'keydown'))
  }
  expect(touch).toHaveBeenCalledTimes(3)
  stop()
  target.dispatchEvent(new Event('keydown'))
  expect(touch).toHaveBeenCalledTimes(3)
})

test('two minutes before the session idles out, a warning with Stay signed in, which touches at once', async () => {
  const start = Date.parse('2026-10-05T12:00:00Z')
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
  vi.setSystemTime(start)
  const idleExpiresAt = new Date(start + 30 * 60_000).toISOString()
  const touched: number[] = []
  const { store } = await fakeStore({
    state: untagged,
    session: { idleExpiresAt },
    answers: {
      'POST /api/session/touch': () => {
        touched.push(Date.now())
        return { status: 204, body: undefined }
      },
    },
  })
  render(
    <StoreProvider store={store}>
      <Activity />
    </StoreProvider>,
  )
  expect(screen.queryByText('Stay signed in')).toBeNull()
  // nothing done for 28 minutes and a second
  await act(async () => {
    vi.setSystemTime(start + 28 * 60_000 + 1000)
    vi.advanceTimersByTime(1000)
  })
  await act(async () => {
    await Promise.resolve()
  })
  const stay = await screen.findByRole('button', { name: 'Stay signed in' })
  expect(screen.getByText(/the session ends in 1 min/)).toBeTruthy()
  expect(touched).toEqual([])
  fireEvent.click(stay)
  await act(async () => {
    await Promise.resolve()
  })
  expect(touched).toHaveLength(1)
})

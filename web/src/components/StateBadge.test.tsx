import { render } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import { routeStateOrder } from '../gen/words.gen'
import { LevelBadge, StateBadge, StatusBadge } from './StateBadge'

describe('StateBadge', () => {
  test.each(routeStateOrder)('%s: an icon and the word, never the colour alone', (state) => {
    const { container } = render(<StateBadge state={state} />)
    expect(container.querySelector('svg')).not.toBeNull()
    expect(container.textContent).toBe(state)
  })

  // Every state of present.RouteStateOrder has a look of its own.
  test.each([
    ['active', 'status-ok'],
    ['unreachable', 'status-fail'],
    ['withdrawn', 'status-warn'],
    ['conflict', 'status-fail'],
    ['no-zone', 'status-idle'],
    ['held', 'status-idle'],
    ['rejected', 'status-warn'],
    ['frozen', 'status-idle'],
  ])('%s is %s', (state, tone) => {
    const { container } = render(<StateBadge state={state} />)
    expect(container.querySelector('.status')?.classList.contains(tone)).toBe(true)
  })

  test('a state of a newer daemon is shown as it is, as untrusted text', () => {
    const { container } = render(<StateBadge state={'odd‮'} />)
    expect(container.textContent).toBe('odd⟨U+202E⟩')
    expect(container.querySelector('.status')?.classList.contains('status-idle')).toBe(true)
  })
})

describe('LevelBadge', () => {
  test.each([
    ['ok', 'status-ok'],
    ['warn', 'status-warn'],
    ['fail', 'status-fail'],
    ['error', 'status-fail'],
    ['skipped', 'status-idle'],
    ['info', 'status-info'],
  ])('%s', (level, tone) => {
    const { container } = render(<LevelBadge level={level} />)
    expect(container.querySelector('.status')?.classList.contains(tone)).toBe(true)
    expect(container.textContent).toBe(level)
  })

  test('with words of its own', () => {
    const { container } = render(<LevelBadge level="skipped">skipped: an earlier step failed</LevelBadge>)
    expect(container.textContent).toBe('skipped: an earlier step failed')
  })
})

test('StatusBadge takes the icon of its tone', () => {
  const { container } = render(<StatusBadge tone="info">waits for approval</StatusBadge>)
  expect(container.querySelector('svg')).not.toBeNull()
  expect(container.textContent).toBe('waits for approval')
})
